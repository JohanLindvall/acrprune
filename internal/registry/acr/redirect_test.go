package acr

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/opencontainers/go-digest"

	"github.com/JohanLindvall/crprune/internal/registry"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) Do(req *http.Request) (*http.Response, error) { return f(req) }

// oversizedBody is finite so a regression still finishes, while counting the
// bytes actually read rather than merely checking for a late size error.
type oversizedBody struct {
	read   int
	closed bool
}

func (b *oversizedBody) Read(p []byte) (int, error) {
	n := min(len(p), 2*registry.MaxDocumentSize-b.read)
	if n == 0 {
		return 0, io.EOF
	}
	for i := range n {
		p[i] = ' '
	}
	b.read += n
	return n, nil
}

func (b *oversizedBody) Close() error { b.closed = true; return nil }

func TestResponseLimitPrecedesSDKBodyDownload(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &oversizedBody{}
			transport := documentTransport{responseTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: body, Request: req}, nil
			})}
			client, err := azcontainerregistry.NewClient("https://example.azurecr.io", nil, &azcontainerregistry.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.NewListRepositoriesPager(nil).NextPage(t.Context())
			if err == nil || !strings.Contains(err.Error(), "document exceeds") || body.read != registry.MaxDocumentSize+1 || !body.closed {
				t.Fatalf("error=%v, bytes read=%d, closed=%t; want refusal at the response size limit", err, body.read, body.closed)
			}
		})
	}
}

type testCredential struct{}

func (testCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-entra-token"}, nil
}

func TestMalformedTokenResponsesFailWithoutPanic(t *testing.T) {
	for _, path := range []string{"/oauth2/token", "/oauth2/exchange"} {
		for _, raw := range []string{"", "null", "{}", `{"access_token":null}`, `{"access_token":""}`, `{"access_token":"  "}`, `{"access_token":42}`, `{"Access_Token":"secret"}`, `{"access_token":"secret"} trailing`} {
			if path == "/oauth2/exchange" {
				raw = strings.ReplaceAll(strings.ReplaceAll(raw, "access_token", "refresh_token"), "Access_Token", "Refresh_Token")
			}
			t.Run(path+"/"+raw, func(t *testing.T) {
				fake := newFakeACR()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path {
						_, _ = io.WriteString(w, raw)
						return
					}
					fake.ServeHTTP(w, r)
				}))
				defer server.Close()
				retry := &testRetry{}
				opts := Options{Endpoint: server.URL, PageSize: 10}
				if path == "/oauth2/exchange" {
					opts.Credential = testCredential{}
				}
				b, err := newBackend(opts, retry.policy())
				if err != nil {
					t.Fatal(err)
				}
				_, err = b.ListRepositories(t.Context())
				if err == nil || !strings.Contains(err.Error(), "token response") || strings.Contains(err.Error(), "secret") || len(retry.waited()) != 0 {
					t.Fatalf("error=%v, waits=%v; want a redacted permanent token error", err, retry.waited())
				}
			})
		}
	}
}

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

func TestMetadataReadRetriesTruncatedBody(t *testing.T) {
	fake := newFakeACR()
	fake.repositories = []string{"app"}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acr/v1/_catalog" && r.Header.Get("Authorization") != "" && attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "{")
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
	got, err := b.ListRepositories(t.Context())
	if err != nil || len(got) != 1 || got[0] != "app" || attempts.Load() != 2 || len(retry.waited()) != 1 {
		t.Fatalf("repositories=%v, error=%v, attempts=%d, waits=%v", got, err, attempts.Load(), retry.waited())
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
