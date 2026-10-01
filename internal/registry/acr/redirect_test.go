package acr

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/JohanLindvall/crprune/internal/registry"
)

func TestManifestReadRetriesTruncatedBody(t *testing.T) {
	fake := newFakeACR()
	raw := []byte(`{"schemaVersion":2,"manifests":[]}`)
	digest := digest.FromBytes(raw).String()
	fake.documents[digest] = string(raw)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v2/app/manifests/"+digest && r.Header.Get("Authorization") != "" && attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "{") // success headers, then a cut-off document
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	retry := &testRetry{}
	b, err := newBackend(Options{Endpoint: server.URL, PageSize: 10}, retry.policy())
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.GetManifest(t.Context(), "app", digest)
	if err != nil || !bytes.Equal(got, raw) || attempts.Load() != 2 || len(retry.waited()) != 1 {
		t.Fatalf("content = %q, error = %v, attempts = %d, waits = %v", got, err, attempts.Load(), retry.waited())
	}
}

// An authenticated DELETE redirected as a GET must never count as deleted.
// A 307/308 to another origin must not forward the mutation or token either.
func TestDeleteRejectsUnsafeRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var forwarded atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer target.Close()
			fake := newFakeACR()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && r.Header.Get("Authorization") != "" {
					http.Redirect(w, r, target.URL+"/deleted", status)
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer server.Close()
			b, err := New(Options{Endpoint: server.URL, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			err = b.DeleteManifest(t.Context(), &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}})
			if err == nil {
				t.Error("unsafe redirect reported a successful deletion")
			}
			if forwarded.Load() != 0 {
				t.Error("forwarded a deletion or its credentials to another origin")
			}
		})
	}
}
