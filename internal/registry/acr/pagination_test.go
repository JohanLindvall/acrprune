package acr

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/crprune/internal/registry"
)

func TestListingsValidateLinksBeforeSDKDecoding(t *testing.T) {
	for _, link := range []string{"broken", "<unfinished", `<>; rel=next`, `<https://elsewhere.example/steal>; rel=next`} {
		t.Run(link, func(t *testing.T) {
			fake := newFakeACR()
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/acr/v1/") && r.Header.Get("Authorization") != "" {
					pages.Add(1)
					w.Header().Set("Link", link)
					writeJSON(w, map[string]any{"repositories": []string{"app"}})
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer server.Close()
			b, err := New(Options{Endpoint: server.URL, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.ListRepositories(t.Context()); err == nil || pages.Load() != 1 {
				t.Fatalf("error=%v, pages=%d; want refusal before another page", err, pages.Load())
			}
		})
	}
}

func TestListingAcceptsAbsoluteAndQueryOnlyNextLinks(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(fmt.Sprint(absolute), func(t *testing.T) {
			fake := newFakeACR()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/acr/v1/_catalog" && r.Header.Get("Authorization") != "" {
					if r.URL.Query().Get("page") == "2" {
						writeJSON(w, map[string]any{"repositories": []string{"b"}})
						return
					}
					target := "?page=2"
					if absolute {
						target = "http://" + r.Host + r.URL.Path + target
					}
					w.Header().Add("Link", "<"+target+">; rel=next")
					writeJSON(w, map[string]any{"repositories": []string{"a"}})
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer server.Close()
			b, err := New(Options{Endpoint: server.URL, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			got, err := b.ListRepositories(t.Context())
			if err != nil || strings.Join(got, ",") != "a,b" {
				t.Fatalf("repositories=%v, error=%v", got, err)
			}
		})
	}
}

func TestListingResolvesNextLinkAtRedirectedURL(t *testing.T) {
	fake := newFakeACR()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			switch r.URL.Path {
			case "/acr/v1/_catalog":
				http.Redirect(w, r, "/acr/v1/relocated/_catalog", http.StatusTemporaryRedirect)
				return
			case "/acr/v1/relocated/_catalog":
				if r.URL.Query().Get("page") == "2" {
					writeJSON(w, map[string]any{"repositories": []string{"b"}})
				} else {
					w.Header().Set("Link", "<?page=2>; rel=next")
					writeJSON(w, map[string]any{"repositories": []string{"a"}})
				}
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	b, err := New(Options{Endpoint: server.URL, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.ListRepositories(t.Context())
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("repositories=%v, error=%v", got, err)
	}
}

func TestListingsRejectRepeatedPages(t *testing.T) {
	for _, operation := range []string{"repositories", "manifests", "tags"} {
		t.Run(operation, func(t *testing.T) {
			fake := newFakeACR()
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" || r.URL.Path == "/oauth2/token" {
					fake.ServeHTTP(w, r)
					return
				}
				// Stop the unfixed implementation rather than hang the test.
				if pages.Add(1) > 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Link", "<"+r.URL.Path+"?last=app>; rel=\"next\"")
				writeJSON(w, map[string]any{operation: []any{}})
			}))
			defer server.Close()
			b, err := New(Options{Endpoint: server.URL, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "repositories":
				_, err = b.ListRepositories(t.Context())
			case "manifests":
				err = b.ListManifests(t.Context(), "app", func(registry.Attributes) error { return nil })
			case "tags":
				_, err = b.LockedTags(t.Context(), "app")
			}
			if err == nil || !strings.Contains(err.Error(), "pagination repeated") || pages.Load() != 2 {
				t.Fatalf("listing made %d requests: %v; want a pagination error after two pages", pages.Load(), err)
			}
		})
	}
}
