package ghcr

import (
	"context"
	"errors"
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

// TestRegistryRefusesForeignChallenge: a live challenge naming a token
// endpoint off the registry's origin, or another scheme, gets no GitHub
// token.
func TestRegistryRefusesForeignChallenge(t *testing.T) {
	for _, challenge := range []string{
		`Bearer realm="https://evil.example/token",service="ghcr.io"`,
		`Basic realm="ghcr.io"`,
	} {
		f, b := newFakeGitHub(t, true)
		digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
		f.before = func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/v2/") {
				return false
			}
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		if _, err := b.GetManifest(ctx, "app", digest); err == nil || !strings.Contains(err.Error(), "authenticat") {
			t.Errorf("%s: error = %v, want the challenge refused", challenge, err)
		}
		if got := f.tokenExchanges(); got != 0 {
			t.Errorf("%s: the token was exchanged %d times", challenge, got)
		}
	}
}

// TestTokenExchangeWaitHonorsContext: a request waiting for another's token
// exchange stops waiting when its context ends, and the exchange completes
// for the other.
func TestTokenExchangeWaitHonorsContext(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	first := f.push("app", manifestDoc("a"), time.Now(), "v1")
	other := f.push("team/api", manifestDoc("b"), time.Now(), "v1")
	if _, err := b.GetManifest(ctx, "app", first); err != nil { // learns the token endpoint
		t.Fatal(err)
	}
	exchanging, release := make(chan struct{}), make(chan struct{})
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/token" && strings.Contains(r.URL.Query().Get("scope"), "team/api") {
			close(exchanging)
			<-release
		}
		return false
	}
	held := make(chan error, 1)
	go func() {
		_, err := b.GetManifest(ctx, "team/api", other)
		held <- err
	}()
	<-exchanging
	waiting, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := b.GetManifest(waiting, "team/api", other); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting request: error = %v, want its deadline", err)
	}
	close(release)
	if err := <-held; err != nil {
		t.Errorf("the exchanging request failed: %v", err)
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
