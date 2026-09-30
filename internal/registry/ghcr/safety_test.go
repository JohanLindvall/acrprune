package ghcr

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRedirectCredentialBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, target, method string
		blocked, auth        bool
	}{
		{"same origin", "https://ghcr.io/blob", "GET", false, true},
		{"subdomain", "https://storage.ghcr.io/blob", "GET", false, false},
		{"different port", "https://ghcr.io:8443/blob", "GET", false, false},
		{"downgrade", "http://ghcr.io/blob", "GET", true, false},
		{"userinfo", "https://user:secret@ghcr.io/blob", "GET", true, false},
		{"mutation", "https://elsewhere.test/blob", "DELETE", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 307, Header: http.Header{"Location": {test.target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				if got := r.Header.Get("Authorization") != ""; got != test.auth {
					t.Errorf("authorization forwarded = %v, want %v", got, test.auth)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
			})}
			b := &Backend{client: client, maxAttempts: 1, logger: slog.New(slog.DiscardHandler)}
			req, err := http.NewRequest(test.method, "https://ghcr.io/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer private")
			resp, err := b.send(req)
			if (err != nil) != test.blocked {
				t.Fatalf("error = %v, blocked=%v", err, test.blocked)
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			if test.blocked && calls != 1 {
				t.Fatal("unsafe redirect reached its target")
			}
		})
	}
}

func TestPaginationRejectsCycles(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Link", "<"+b.apiURL.String()+r.URL.RequestURI()+">; rel=\"next\"")
		_, _ = io.WriteString(w, `[]`)
		return true
	}
	if _, err := b.ListRepositories(ctx); err == nil || !strings.Contains(err.Error(), "repeated a page") {
		t.Fatalf("error = %v", err)
	}
}

func TestTokenRealmQueryAndFreshCache(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+b.registryURL.String()+`/token?existing=1",service="ghcr.io"`)
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		if r.URL.Path == "/token" && r.URL.Query().Get("existing") != "1" {
			t.Error("realm query was lost")
		}
		return false
	}
	if _, err := b.GetManifest(context.Background(), "app", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := b.fetchRegistryToken(ctx, "repository:"+b.owner+"/app:pull", "stale"); err != nil {
		t.Fatal(err)
	}
	if f.tokenExchanges() != 1 {
		t.Fatal("an already renewed token was exchanged again")
	}
}

func TestRateLimitDurationsDoNotOverflow(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": {"9223372036854775807"}}}
	if got := rateLimitDelay(resp, time.Now()); got != time.Duration(math.MaxInt64) {
		t.Fatalf("delay = %v", got)
	}
	if backoff(1000) != 30*time.Second {
		t.Fatal("backoff overflowed")
	}
	u, _ := url.Parse("https://user:pass@host/blob?secret=x#secret")
	if got := redact(u); got != "https://host/blob" {
		t.Fatalf("redact = %q", got)
	}
}
