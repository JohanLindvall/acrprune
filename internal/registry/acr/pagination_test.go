package acr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

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
