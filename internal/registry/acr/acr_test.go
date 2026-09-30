package acr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

// fakeACR emulates the ACR data-plane endpoints the backend uses, including
// the challenge the client answers with an anonymous token exchange before
// its first real request.
type fakeACR struct {
	mu           sync.Mutex
	repositories []string
	manifests    map[string][]any  // repository -> manifest attribute objects
	documents    map[string]string // digest -> manifest document
	tags         map[string][]any  // repository -> tag attribute objects
	fail         map[string]int    // "METHOD path" -> status to fail with
	requests     []string          // "METHOD path" of authenticated requests
	bodies       map[string]string // "METHOD path" -> request body
	accept       map[string]string // "METHOD path" -> Accept header
}

func newFakeACR() *fakeACR {
	return &fakeACR{
		manifests: map[string][]any{},
		documents: map[string]string{},
		tags:      map[string][]any{},
		fail:      map[string]int{},
		bodies:    map[string]string{},
		accept:    map[string]string{},
	}
}

func (f *fakeACR) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if path == "/oauth2/token" {
		writeJSON(w, map[string]string{"access_token": "acr-token"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer acr-token" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/oauth2/token",service="fake.azurecr.io",scope="registry:catalog:*"`, r.Host))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	key := r.Method + " " + path
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, key)
	f.bodies[key] = string(body)
	f.accept[key] = strings.Join(r.Header.Values("Accept"), ",")
	if status := f.fail[key]; status != 0 {
		w.WriteHeader(status)
		writeJSON(w, map[string]any{"errors": []map[string]string{{"code": "DENIED", "message": "denied by the fake"}}})
		return
	}

	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	name := func(i int) string {
		unescaped, _ := url.PathUnescape(segments[i])
		return unescaped
	}
	switch {
	case r.Method == http.MethodGet && path == "/acr/v1/_catalog":
		names := make([]any, len(f.repositories))
		for i, repository := range f.repositories {
			names[i] = repository
		}
		f.page(w, r, "repositories", names)
	case r.Method == http.MethodGet && len(segments) == 4 && segments[0] == "acr" && segments[3] == "_manifests":
		f.page(w, r, "manifests", f.manifests[name(2)])
	case r.Method == http.MethodGet && len(segments) == 4 && segments[0] == "acr" && segments[3] == "_tags":
		f.page(w, r, "tags", f.tags[name(2)])
	case r.Method == http.MethodGet && len(segments) == 4 && segments[0] == "v2" && segments[2] == "manifests":
		document, ok := f.documents[name(3)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", name(3))
		_, _ = io.WriteString(w, document)
	case r.Method == http.MethodDelete && (segments[0] == "v2" || segments[0] == "acr"):
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch && segments[0] == "acr":
		writeJSON(w, map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// page writes one page of a listing: n items starting at the "start" query
// parameter, with a Link header to the next page as ACR sends one.
func (f *fakeACR) page(w http.ResponseWriter, r *http.Request, field string, items []any) {
	if items == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"errors": []map[string]string{{"code": "NAME_UNKNOWN", "message": "repository not found"}}})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	end := min(start+n, len(items))
	if end < len(items) {
		w.Header().Set("Link", fmt.Sprintf(`<%s?n=%d&start=%d>; rel="next"`, r.URL.EscapedPath(), n, end))
	}
	writeJSON(w, map[string]any{field: items[start:end]})
}

func (f *fakeACR) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestBackend(t *testing.T, f *fakeACR, pageSize int) *Backend {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := azcontainerregistry.NewClient(srv.URL, nil, &azcontainerregistry.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New(client, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestNewRejectsUnusablePageSize(t *testing.T) {
	for _, pageSize := range []int{0, -1, 1 << 40} {
		if _, err := New(nil, pageSize); err == nil {
			t.Errorf("New should reject page size %d", pageSize)
		}
	}
}

func TestListRepositories(t *testing.T) {
	f := newFakeACR()
	f.repositories = []string{"app", "team/api", "tools"}
	b := newTestBackend(t, f, 2)

	got, err := b.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, f.repositories) {
		t.Errorf("ListRepositories = %v, want %v across two pages", got, f.repositories)
	}
}

// TestListRepositoriesPermissionHint: on ABAC registries listing the catalog
// needs its own role, which the error names.
func TestListRepositoriesPermissionHint(t *testing.T) {
	f := newFakeACR()
	f.fail["GET /acr/v1/_catalog"] = http.StatusForbidden
	_, err := newTestBackend(t, f, 10).ListRepositories(context.Background())
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "Catalog Lister") {
		t.Errorf("error = %v, want a permission error naming the Catalog Lister role", err)
	}
}

func TestListManifests(t *testing.T) {
	updated := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	f := newFakeACR()
	f.manifests["team/app"] = []any{
		map[string]any{
			"digest":         "sha256:aaa",
			"tags":           []any{"v1", nil, "latest"},
			"lastUpdateTime": updated.Format(time.RFC3339),
			"architecture":   "arm64",
			"os":             "linux",
			"changeableAttributes": map[string]any{
				"deleteEnabled": false,
				"writeEnabled":  true,
			},
		},
		map[string]any{"digest": "sha256:bbb"},
		map[string]any{"digest": "sha256:ccc", "changeableAttributes": map[string]any{"deleteEnabled": true, "writeEnabled": true}},
	}
	b := newTestBackend(t, f, 2)

	var listed []registry.Attributes
	err := b.ListManifests(context.Background(), "team/app", func(attrs registry.Attributes) error {
		listed = append(listed, attrs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d manifests, want 3 across two pages", len(listed))
	}
	first := listed[0]
	if first.Digest != "sha256:aaa" || !slices.Equal(first.Tags, []string{"v1", "latest"}) ||
		!first.LastUpdated.Equal(updated) || first.Architecture != "arm64" || first.OS != "linux" || !first.Locked {
		t.Errorf("attributes = %+v", first)
	}
	if second := listed[1]; second.Digest != "sha256:bbb" || len(second.Tags) != 0 || !second.LastUpdated.IsZero() || second.Locked {
		t.Errorf("bare attributes = %+v", second)
	}
	if listed[2].Locked {
		t.Error("a manifest with delete and write enabled is not locked")
	}

	// An error from fn stops the listing.
	boom := errors.New("boom")
	if err := b.ListManifests(context.Background(), "team/app", func(registry.Attributes) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("fn error = %v, want %v", err, boom)
	}
}

func TestListManifestsMissingRepository(t *testing.T) {
	b := newTestBackend(t, newFakeACR(), 10)
	err := b.ListManifests(context.Background(), "gone", func(registry.Attributes) error { return nil })
	if !registry.IsNotFound(err) {
		t.Errorf("error = %v, want not found", err)
	}
}

func TestGetManifest(t *testing.T) {
	f := newFakeACR()
	f.documents["sha256:aaa"] = `{"schemaVersion": 2}`
	b := newTestBackend(t, f, 10)

	raw, err := b.GetManifest(context.Background(), "team/app", "sha256:aaa")
	if err != nil || string(raw) != `{"schemaVersion": 2}` {
		t.Fatalf("GetManifest = %q, %v", raw, err)
	}
	accept := f.accept["GET /v2/team%2Fapp/manifests/sha256:aaa"]
	for _, mediaType := range strings.Split(registry.ManifestMediaTypes, ",") {
		if !strings.Contains(accept, mediaType) {
			t.Errorf("Accept %q does not offer %s", accept, mediaType)
		}
	}

	if _, err := b.GetManifest(context.Background(), "team/app", "sha256:gone"); !registry.IsNotFound(err) {
		t.Errorf("missing manifest error = %v, want not found", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFakeACR()
	b := newTestBackend(t, f, 10)
	ctx := context.Background()

	m := &registry.Manifest{Repository: "team/app", Attributes: registry.Attributes{Digest: "sha256:aaa"}}
	if err := b.DeleteManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteRepository(ctx, "team/app"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /v2/team%2Fapp/manifests/sha256:aaa", "DELETE /acr/v1/team%2Fapp"}
	if got := f.requested(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}

	f.fail["DELETE /acr/v1/other"] = http.StatusForbidden
	if err := b.DeleteRepository(ctx, "other"); !registry.IsPermissionError(err) {
		t.Errorf("error = %v, want a permission error", err)
	}
	for _, key := range want {
		f.fail[key] = http.StatusNotFound
	}
	if err := b.DeleteManifest(ctx, m); err != nil {
		t.Errorf("already deleted manifest: %v", err)
	}
	if err := b.DeleteRepository(ctx, "team/app"); err != nil {
		t.Errorf("already deleted repository: %v", err)
	}
}

func TestLocks(t *testing.T) {
	f := newFakeACR()
	f.tags["app"] = []any{
		map[string]any{"name": "v1", "changeableAttributes": map[string]any{"deleteEnabled": false}},
		map[string]any{"name": "v2", "changeableAttributes": map[string]any{"writeEnabled": false}},
		map[string]any{"name": "v3", "changeableAttributes": map[string]any{"deleteEnabled": true, "writeEnabled": true}},
		map[string]any{"name": "v4"},
	}
	b := newTestBackend(t, f, 2)
	ctx := context.Background()

	locked, err := b.LockedTags(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 2 || !locked["v1"] || !locked["v2"] {
		t.Errorf("LockedTags = %v, want v1 and v2", locked)
	}

	if err := b.UnlockManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: "sha256:aaa"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.UnlockTag(ctx, "app", "v1"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"PATCH /acr/v1/app/_manifests/sha256:aaa", "PATCH /acr/v1/app/_tags/v1"} {
		var body map[string]bool
		if err := json.Unmarshal([]byte(f.bodies[key]), &body); err != nil {
			t.Fatalf("%s: body %q: %v", key, f.bodies[key], err)
		}
		if !body["deleteEnabled"] || !body["writeEnabled"] {
			t.Errorf("%s: body = %v, want delete and write enabled", key, body)
		}
	}

	if _, err := b.LockedTags(ctx, "gone"); !registry.IsNotFound(err) {
		t.Errorf("LockedTags of a missing repository = %v, want not found", err)
	}
}

func TestLocked(t *testing.T) {
	enabled, disabled := true, false
	tests := []struct {
		name                string
		canDelete, canWrite *bool
		want                bool
	}{
		{"no attributes", nil, nil, false},
		{"fully enabled", &enabled, &enabled, false},
		{"delete disabled", &disabled, nil, true},
		{"write disabled", nil, &disabled, true},
	}
	for _, tt := range tests {
		if got := locked(tt.canDelete, tt.canWrite); got != tt.want {
			t.Errorf("%s: locked = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAttributesOfNil(t *testing.T) {
	if got := attributes(nil); got.Digest != "" {
		t.Errorf("attributes(nil) = %+v, want no digest for the caller to reject", got)
	}
}

// fakePager pages through the given values, failing on a value of -1.
func fakePager(values []int) *runtime.Pager[int] {
	i := 0
	return runtime.NewPager(runtime.PagingHandler[int]{
		More: func(int) bool { return i < len(values) },
		Fetcher: func(ctx context.Context, _ *int) (int, error) {
			v := values[i]
			i++
			if v == -1 {
				return 0, &azcore.ResponseError{StatusCode: http.StatusForbidden}
			}
			return v, nil
		},
	})
}

func TestForEachPage(t *testing.T) {
	var got []int
	err := forEachPage(context.Background(), fakePager([]int{1, 2, 3}), func(page int) error {
		got = append(got, page)
		return nil
	})
	if err != nil || !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("forEachPage = %v, %v", got, err)
	}

	// A paging error aborts the walk, classified for the caller.
	got = nil
	err = forEachPage(context.Background(), fakePager([]int{1, -1, 3}), func(page int) error {
		got = append(got, page)
		return nil
	})
	if !registry.IsPermissionError(err) || len(got) != 1 {
		t.Errorf("paging error not propagated: %v, %v", got, err)
	}

	// An fn error aborts the walk too.
	boom := errors.New("boom")
	err = forEachPage(context.Background(), fakePager([]int{1, 2}), func(int) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("fn error not propagated: %v", err)
	}
}

func TestWrap(t *testing.T) {
	azErr := &azcore.ResponseError{StatusCode: http.StatusNotFound}
	wrapped := wrap(fmt.Errorf("context: %w", azErr))
	if !registry.IsNotFound(wrapped) {
		t.Errorf("wrap(%v) should classify as not found", wrapped)
	}
	var original *azcore.ResponseError
	if !errors.As(wrapped, &original) {
		t.Error("the SDK's error should stay in the chain")
	}
	plain := errors.New("plain")
	if wrap(plain) != plain || wrap(nil) != nil {
		t.Error("other errors should pass through unchanged")
	}
}
