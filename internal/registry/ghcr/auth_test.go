package ghcr

import (
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseParams(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{`realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/app:pull"`,
			map[string]string{"realm": "https://ghcr.io/token", "service": "ghcr.io", "scope": "repository:o/app:pull"}},
		// Quoted values may hold commas and escaped quotes; keys are
		// case-insensitive; unquoted values end at a comma.
		{`Realm="r", scope="repository:o/app:pull,push", error=invalid_token, note="say \"hi\""`,
			map[string]string{"realm": "r", "scope": "repository:o/app:pull,push", "error": "invalid_token", "note": `say "hi"`}},
		{`realm="unterminated`, map[string]string{"realm": "unterminated"}},
		{``, map[string]string{}},
		{`novalue`, map[string]string{}},
	}
	for _, tt := range tests {
		if got := parseParams(tt.in); !maps.Equal(got, tt.want) {
			t.Errorf("parseParams(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestParseChallenge: the GitHub token goes to the challenge's realm, which
// must therefore be the registry's own origin.
func TestParseChallenge(t *testing.T) {
	registryURL, _ := url.Parse("https://ghcr.io")
	b := &Backend{registryURL: registryURL}

	realm, service, err := b.parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/app:pull"`)
	if err != nil || realm != "https://ghcr.io/token" || service != "ghcr.io" {
		t.Errorf("parseChallenge = %q, %q, %v", realm, service, err)
	}

	for _, bad := range []string{
		`Basic realm="ghcr.io"`,
		`Bearer realm="https://evil.example/token",service="ghcr.io"`,
		`Bearer realm="http://ghcr.io/token"`,
		`Bearer service="ghcr.io"`,
		``,
	} {
		if _, _, err := b.parseChallenge(bad); err == nil {
			t.Errorf("parseChallenge(%q) should be refused", bad)
		}
	}
}

// TestTokenResponseVariants: the token may come as "token" or
// "access_token", and a response without either is an error.
func TestTokenResponseVariants(t *testing.T) {
	for body, wantErr := range map[string]bool{
		`{"access_token": "registry-token"}`: false,
		`{"token": "registry-token"}`:        false,
		`{}`:                                 true,
		`not json`:                           true,
	} {
		f, b := newFakeGitHub(t, true)
		digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
		f.before = func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == "/token":
				_, _ = w.Write([]byte(body))
				return true
			case strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("Authorization") == "Bearer registry-token":
				_, _ = w.Write([]byte(`{"schemaVersion": 2}`))
				return true
			}
			return false
		}
		if _, err := b.GetManifest(ctx, "app", digest); (err != nil) != wantErr {
			t.Errorf("token response %s: error = %v, want error %v", body, err, wantErr)
		}
	}
}

// TestConcurrentRequestsShareTokenExchange: parallel downloads of one
// repository exchange the token once.
func TestConcurrentRequestsShareTokenExchange(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	var digests []string
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		digests = append(digests, f.push("app", manifestDoc(name), time.Now(), name))
	}
	// Hold the token exchange until every download has been challenged, so
	// that they all need the token at once. A download starting after the
	// first challenge fetches the token without being challenged itself, so
	// the wait is bounded.
	var challenged atomic.Int32
	allChallenged := make(chan struct{})
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("Authorization") == "":
			if challenged.Add(1) == int32(len(digests)) {
				close(allChallenged)
			}
		case r.URL.Path == "/token":
			select {
			case <-allChallenged:
			case <-time.After(time.Second):
			}
		}
		return false
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(digests))
	for _, digest := range digests {
		wg.Go(func() {
			_, err := b.GetManifest(ctx, "app", digest)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.tokenExchanges(); got != 1 {
		t.Errorf("token exchanges = %d, want 1 shared by all downloads", got)
	}
}
