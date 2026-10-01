package acr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	godigest "github.com/opencontainers/go-digest"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
)

// fakeACR emulates the ACR data-plane endpoints the backend uses, including
// the challenge the client answers with an anonymous token exchange. As ACR
// does, it issues access tokens valid for one repository and action — the
// scope its challenge names — so a client alternating between scopes pays a
// token exchange for each switch.
type fakeACR struct {
	mu           sync.Mutex
	repositories []string
	manifests    map[string][]any  // repository -> manifest attribute objects
	documents    map[string]string // digest -> manifest document
	tags         map[string][]any  // repository -> tag attribute objects
	fail         map[string]int    // "METHOD path" -> status to fail with
	flaky        map[string][]int  // "METHOD path" -> statuses to fail with first, in order
	retryAfter   string            // Retry-After header of flaky failures
	requests     []string          // "METHOD path" of authenticated requests
	bodies       map[string]string // "METHOD path" -> request body
	accept       map[string]string // "METHOD path" -> Accept header
	exchanges    map[string]int    // scope -> access tokens issued
}

func newFakeACR() *fakeACR {
	return &fakeACR{
		manifests: map[string][]any{},
		documents: map[string]string{},
		tags:      map[string][]any{},
		fail:      map[string]int{},
		flaky:     map[string][]int{},
		bodies:    map[string]string{},
		accept:    map[string]string{},
		exchanges: map[string]int{},
	}
}

// token returns the access token the fake issues for a scope.
func token(scope string) string {
	return url.QueryEscape(scope)
}

// scope returns the token scope ACR requires for a request.
func scope(method string, segments []string, name func(int) string) string {
	switch {
	case len(segments) >= 3 && segments[0] == "acr" && segments[2] == "_catalog":
		return "registry:catalog:*"
	case len(segments) >= 3 && segments[0] == "acr":
		switch method {
		case http.MethodPatch:
			return "repository:" + name(2) + ":metadata_write"
		case http.MethodDelete:
			return "repository:" + name(2) + ":delete"
		}
		return "repository:" + name(2) + ":metadata_read"
	case len(segments) >= 2 && segments[0] == "v2":
		if method == http.MethodDelete {
			return "repository:" + name(1) + ":delete"
		}
		return "repository:" + name(1) + ":pull"
	}
	return "none"
}

func (f *fakeACR) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	body, _ := io.ReadAll(r.Body)
	if path == "/oauth2/token" {
		form, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		f.exchanges[form.Get("scope")]++
		f.mu.Unlock()
		writeJSON(w, map[string]string{"access_token": token(form.Get("scope"))})
		return
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	name := func(i int) string {
		unescaped, _ := url.PathUnescape(segments[i])
		return unescaped
	}
	required := scope(r.Method, segments, name)
	if r.Header.Get("Authorization") != "Bearer "+token(required) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/oauth2/token",service="fake.azurecr.io",scope="%s"`, r.Host, required))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	key := r.Method + " " + path
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, key)
	f.bodies[key] = string(body)
	f.accept[key] = strings.Join(r.Header.Values("Accept"), ",")
	if statuses := f.flaky[key]; len(statuses) > 0 {
		f.flaky[key] = statuses[1:]
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(statuses[0])
		return
	}
	if status := f.fail[key]; status != 0 {
		w.WriteHeader(status)
		writeJSON(w, map[string]any{"errors": []map[string]string{{"code": "DENIED", "message": "denied by the fake"}}})
		return
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
	case len(segments) == 5 && segments[0] == "acr" && (segments[3] == "_manifests" || segments[3] == "_tags"):
		f.properties(w, r.Method, name(2), segments[3], name(4), body)
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
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// properties serves and updates the changeable attributes of a manifest
// (kind _manifests, by digest) or tag (kind _tags, by name). f.mu must be
// held.
func (f *fakeACR) properties(w http.ResponseWriter, method, repository, kind, reference string, body []byte) {
	items, field, key := f.manifests[repository], "manifest", "digest"
	if kind == "_tags" {
		items, field, key = f.tags[repository], "tag", "name"
	}
	i := slices.IndexFunc(items, func(item any) bool { return item.(map[string]any)[key] == reference })
	if i < 0 {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"errors": []map[string]string{{"code": "MANIFEST_UNKNOWN", "message": "not found"}}})
		return
	}
	item := items[i].(map[string]any)
	if method == http.MethodPatch {
		var update map[string]any
		if err := json.Unmarshal(body, &update); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		attrs, _ := item["changeableAttributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
			item["changeableAttributes"] = attrs
		}
		for k, v := range update {
			attrs[k] = v
		}
	}
	writeJSON(w, map[string]any{"registry": "fake.azurecr.io", "imageName": repository, field: item})
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

// tokensIssued returns how many access tokens the fake issued.
func (f *fakeACR) tokensIssued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, count := range f.exchanges {
		n += count
	}
	return n
}

// changeable returns the changeable attributes of a stored manifest or tag.
func (f *fakeACR) changeable(items []any, key, reference string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range items {
		if item := item.(map[string]any); item[key] == reference {
			attrs, _ := item["changeableAttributes"].(map[string]any)
			return attrs
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testRetry retries promptly, recording the delays it would have waited.
type testRetry struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *testRetry) policy() retryPolicy {
	return retryPolicy{maxRetries: 3, delay: time.Second, maxDelay: time.Minute, sleep: func(ctx context.Context, d time.Duration) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.delays = append(r.delays, d)
		return ctx.Err()
	}}
}

func (r *testRetry) waited() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.delays)
}

func newTestBackend(t *testing.T, f *fakeACR, pageSize int) *Backend {
	t.Helper()
	b, _ := newRetryingBackend(t, f, pageSize, nil)
	return b
}

func newRetryingBackend(t *testing.T, f *fakeACR, pageSize int, logger *slog.Logger) (*Backend, *testRetry) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	retry := &testRetry{}
	backend, err := newBackend(Options{Endpoint: srv.URL, PageSize: pageSize, Logger: logger}, retry.policy())
	if err != nil {
		t.Fatal(err)
	}
	return backend, retry
}

func TestNewRejectsUnusableOptions(t *testing.T) {
	for _, pageSize := range []int{0, -1, 1 << 40} {
		if _, err := New(Options{Endpoint: "https://myreg.azurecr.io", PageSize: pageSize}); err == nil {
			t.Errorf("New should reject page size %d", pageSize)
		}
	}
	if _, err := New(Options{PageSize: 10}); err == nil {
		t.Error("New should require an endpoint")
	}
	if b, err := New(Options{Endpoint: "https://myreg.azurecr.io", PageSize: 10}); err != nil || b.metadata == nil || b.updates == nil || b.content == nil {
		t.Errorf("New = %+v, %v", b, err)
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

func TestLockedTags(t *testing.T) {
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
	if _, err := b.LockedTags(ctx, "gone"); !registry.IsNotFound(err) {
		t.Errorf("LockedTags of a missing repository = %v, want not found", err)
	}
}

// TestUnlockRelocks: unlocking enables delete and write, and the Relock it
// returns disables again exactly what was disabled before.
func TestUnlockRelocks(t *testing.T) {
	f := newFakeACR()
	f.manifests["app"] = []any{
		map[string]any{"digest": "sha256:aaa", "changeableAttributes": map[string]any{"deleteEnabled": false, "writeEnabled": true, "readEnabled": true}},
		map[string]any{"digest": "sha256:bbb", "changeableAttributes": map[string]any{"deleteEnabled": true, "writeEnabled": true}},
	}
	f.tags["app"] = []any{
		map[string]any{"name": "v1", "changeableAttributes": map[string]any{"deleteEnabled": true, "writeEnabled": false}},
	}
	b := newTestBackend(t, f, 10)
	ctx := context.Background()
	manifest := func(digest string) *registry.Manifest {
		return &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: digest}}
	}
	enabled := map[string]any{"deleteEnabled": true, "writeEnabled": true}

	relockManifest, err := b.UnlockManifest(ctx, manifest("sha256:aaa"))
	if err != nil || relockManifest == nil {
		t.Fatalf("UnlockManifest = %v, %v", relockManifest, err)
	}
	relockTag, err := b.UnlockTag(ctx, "app", "v1")
	if err != nil || relockTag == nil {
		t.Fatalf("UnlockTag = %v, %v", relockTag, err)
	}
	for _, key := range []string{"PATCH /acr/v1/app/_manifests/sha256:aaa", "PATCH /acr/v1/app/_tags/v1"} {
		var body map[string]any
		if err := json.Unmarshal([]byte(f.bodies[key]), &body); err != nil || !mapsEqual(body, enabled) {
			t.Errorf("%s: body = %v, %v; want delete and write enabled", key, body, err)
		}
	}

	if err := relockManifest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := relockTag(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.changeable(f.manifests["app"], "digest", "sha256:aaa"); !mapsEqual(got, map[string]any{"deleteEnabled": false, "writeEnabled": true, "readEnabled": true}) {
		t.Errorf("relocked manifest = %v, want delete disabled again and nothing else changed", got)
	}
	if got := f.changeable(f.tags["app"], "name", "v1"); !mapsEqual(got, map[string]any{"deleteEnabled": true, "writeEnabled": false}) {
		t.Errorf("relocked tag = %v, want write disabled again", got)
	}

	// Nothing to unlock: not locked, or gone.
	before := len(f.requested())
	for _, digest := range []string{"sha256:bbb", "sha256:gone"} {
		if relock, err := b.UnlockManifest(ctx, manifest(digest)); relock != nil || err != nil {
			t.Errorf("UnlockManifest(%s) = %v, %v; want nothing to relock", digest, relock, err)
		}
	}
	if relock, err := b.UnlockTag(ctx, "app", "gone"); relock != nil || err != nil {
		t.Errorf("UnlockTag(gone) = %v, %v; want nothing to relock", relock, err)
	}
	for _, request := range f.requested()[before:] {
		if strings.HasPrefix(request, http.MethodPatch) {
			t.Errorf("unexpected update %s", request)
		}
	}

	// A lock whose state cannot be read is left alone.
	f.fail["GET /acr/v1/app/_manifests/sha256:aaa"] = http.StatusForbidden
	if relock, err := b.UnlockManifest(ctx, manifest("sha256:aaa")); relock != nil || !registry.IsPermissionError(err) {
		t.Errorf("UnlockManifest = %v, %v; want the permission error", relock, err)
	}
}

// TestFailedUnlockRelocks: an unlock that the registry applied, but whose
// response an interruption cut off, returned no Relock, so the manifest stayed
// unlocked for good. A failed unlock returns its Relock too.
func TestFailedUnlockRelocks(t *testing.T) {
	f := newFakeACR()
	locked := map[string]any{"deleteEnabled": false, "writeEnabled": false}
	f.manifests["app"] = []any{map[string]any{"digest": "sha256:aaa", "changeableAttributes": maps.Clone(locked)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.Header.Get("Authorization") == "" || ctx.Err() != nil {
			f.ServeHTTP(w, r)
			return
		}
		f.ServeHTTP(httptest.NewRecorder(), r) // applied, but interrupted
		cancel()                               // before the response arrives
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	b, err := newBackend(Options{Endpoint: srv.URL, PageSize: 10}, (&testRetry{}).policy())
	if err != nil {
		t.Fatal(err)
	}
	m := &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: "sha256:aaa"}}
	state := func() map[string]any { return f.changeable(f.manifests["app"], "digest", "sha256:aaa") }

	relock, err := b.UnlockManifest(ctx, m)
	if !errors.Is(err, context.Canceled) || relock == nil {
		t.Fatalf("UnlockManifest = %v, %v; want a Relock along with the cancellation", relock != nil, err)
	}
	if got := state(); !mapsEqual(got, map[string]any{"deleteEnabled": true, "writeEnabled": true}) {
		t.Fatalf("attributes %v; the fake should have applied the unlock", got)
	}
	if err := relock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := state(); !mapsEqual(got, locked) {
		t.Errorf("relocked attributes %v, want %v", got, locked)
	}

	f.fail["PATCH /acr/v1/app/_manifests/sha256:aaa"] = http.StatusForbidden
	if relock, err := b.UnlockManifest(context.Background(), m); !registry.IsPermissionError(err) || relock == nil {
		t.Errorf("UnlockManifest = %v, %v; want a Relock along with the refusal", relock != nil, err)
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
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

// registryDigest returns the digest of a document.
func registryDigest(document string) string {
	return godigest.FromString(document).String()
}

// populate stores repositories of manifests in f, each tagged, with
// documents that verify, and returns the repository names. With locked, every
// manifest and tag is locked.
func populate(f *fakeACR, repositories, manifests int, locked bool) []string {
	var names []string
	for r := range repositories {
		name := fmt.Sprintf("team/app%d", r)
		names = append(names, name)
		f.tags[name] = []any{}
		for i := range manifests {
			document := fmt.Sprintf(`{"schemaVersion": 2, "annotations": {"n": "%s-%d"}}`, name, i)
			digest := registryDigest(document)
			tag := fmt.Sprintf("v%d", i)
			f.documents[digest] = document
			f.manifests[name] = append(f.manifests[name], map[string]any{"digest": digest, "tags": []any{tag},
				"changeableAttributes": map[string]any{"deleteEnabled": !locked}})
			f.tags[name] = append(f.tags[name], map[string]any{"name": tag, "digest": digest,
				"changeableAttributes": map[string]any{"writeEnabled": !locked}})
		}
	}
	return names
}

// TestTokenExchangesArePerRepositoryAndAction: listing, downloading, unlocking
// and deleting a repository's manifests in parallel obtain one access token
// per repository and action, however the requests interleave, rather than one
// per switch between actions.
func TestTokenExchangesArePerRepositoryAndAction(t *testing.T) {
	const repositories, manifests = 12, 40
	for _, locked := range []bool{false, true} {
		f := newFakeACR()
		names := populate(f, repositories, manifests, locked)
		b := newTestBackend(t, f, 10)
		reg, err := registry.New(b, nil, 16, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		for _, name := range names {
			contents, found, err := reg.FetchRepositoryManifests(ctx, name, registry.FetchOptions{})
			if err != nil || !found || len(contents.Manifests) != manifests {
				t.Fatalf("%s: fetched %d manifests, %v, %v", name, len(contents.Manifests), found, err)
			}
			all := make([]*registry.Manifest, 0, manifests)
			for _, m := range contents.Manifests {
				all = append(all, m)
			}
			if err := reg.LoadTagLocks(ctx, name, all); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.DeleteManifests(ctx, all, registry.DeleteOptions{Unlock: true}); err != nil {
				t.Fatal(err)
			}
		}
		// metadata_read, pull and delete, and metadata_write to unlock,
		// once each per repository.
		most := 3 * repositories
		if locked {
			most += repositories
			if patched := strings.Count(strings.Join(f.requested(), "\n"), "PATCH"); patched != 2*repositories*manifests {
				t.Errorf("%d updates, want every manifest and tag unlocked", patched)
			}
		}
		if got := f.tokensIssued(); got > most {
			t.Errorf("locked=%v: %d access tokens issued, want at most %d: %v", locked, got, most, f.exchanges)
		}
	}
}

// TestFirstRequestGoesAlone: requests of one action on a repository wait for
// the first to complete, unless their context ends first.
func TestFirstRequestGoesAlone(t *testing.T) {
	var g gates
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		done <- g.do(ctx, "pull", "app", func() error {
			close(started)
			<-release
			return errors.New("first")
		})
	}()
	<-started

	// Another repository or action is not held up.
	if err := g.do(ctx, "pull", "other", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.do(ctx, "delete", "app", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	ran := false
	if err := g.do(canceled, "pull", "app", func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Errorf("a waiter whose context ends = %v (ran %v), want it to give up", err, ran)
	}

	waiter := make(chan error)
	go func() { waiter <- g.do(ctx, "pull", "app", func() error { return nil }) }()
	select {
	case err := <-waiter:
		t.Fatalf("a waiter ran before the first request completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil || err.Error() != "first" {
		t.Errorf("first request = %v", err)
	}
	if err := <-waiter; err != nil {
		t.Errorf("waiter = %v, want it to run once the first request completed, whatever its outcome", err)
	}
}

// retries returns the delay attribute of every "Retrying request" warning
// logs recorded.
func retries(t *testing.T, logs *registrytest.Recorder) []time.Duration {
	t.Helper()
	var delays []time.Duration
	for _, record := range logs.Records() {
		if record.Message != "Retrying request" || record.Level != slog.LevelWarn {
			continue
		}
		record.Attrs(func(a slog.Attr) bool {
			if a.Key == "delay" {
				// The progress display counts down a duration.
				if a.Value.Kind() != slog.KindDuration {
					t.Errorf("delay attribute of kind %v, want a duration", a.Value.Kind())
				}
				delays = append(delays, a.Value.Duration())
			}
			return true
		})
	}
	return delays
}

// TestRetries: throttled and failing requests are retried, waiting as long as
// the registry asks or backing off exponentially, and each retry is logged.
func TestRetries(t *testing.T) {
	f := newFakeACR()
	f.documents["sha256:aaa"] = `{"schemaVersion": 2}`
	f.manifests["app"] = []any{map[string]any{"digest": "sha256:aaa"}}
	logs := &registrytest.Recorder{}
	b, retry := newRetryingBackend(t, f, 10, slog.New(logs))
	ctx := context.Background()

	f.flaky["GET /v2/app/manifests/sha256:aaa"] = []int{http.StatusTooManyRequests, http.StatusTooManyRequests}
	f.retryAfter = "7"
	if raw, err := b.GetManifest(ctx, "app", "sha256:aaa"); err != nil || string(raw) != `{"schemaVersion": 2}` {
		t.Fatalf("GetManifest = %q, %v; want it to succeed on the third attempt", raw, err)
	}
	f.flaky["GET /acr/v1/app/_manifests"] = []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusInternalServerError}
	f.retryAfter = ""
	if err := b.ListManifests(ctx, "app", func(registry.Attributes) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{7 * time.Second, 7 * time.Second, time.Second, 2 * time.Second, 4 * time.Second}
	if got := retry.waited(); !slices.Equal(got, want) {
		t.Errorf("waited %v, want %v", got, want)
	}
	if got := retries(t, logs); !slices.Equal(got, want) {
		t.Errorf("logged retries after %v, want %v", got, want)
	}

	// A registry asking for longer than the maximum delay is retried after
	// the maximum.
	f.flaky["GET /v2/app/manifests/sha256:aaa"] = []int{http.StatusTooManyRequests}
	f.retryAfter = "3600"
	if _, err := b.GetManifest(ctx, "app", "sha256:aaa"); err != nil {
		t.Fatal(err)
	}
	if got := retry.waited(); got[len(got)-1] != time.Minute {
		t.Errorf("waited %v, want the maximum delay", got[len(got)-1])
	}
}

// TestRetriesGiveUp: retries are bounded, and final failures are not retried.
func TestRetriesGiveUp(t *testing.T) {
	f := newFakeACR()
	b, retry := newRetryingBackend(t, f, 10, nil)
	ctx := context.Background()

	f.flaky["DELETE /v2/app/manifests/sha256:aaa"] = []int{503, 503, 503, 503, 503}
	err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: "sha256:aaa"}})
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("error = %v, want the last failure once retries are exhausted", err)
	}
	if got := len(retry.waited()); got != 3 {
		t.Errorf("retried %d times, want 3", got)
	}

	for key, status := range map[string]int{
		"GET /acr/v1/app/_tags":  http.StatusForbidden,
		"GET /acr/v1/app2/_tags": http.StatusNotFound,
	} {
		f.fail[key] = status
	}
	for _, repository := range []string{"app", "app2"} {
		if _, err := b.LockedTags(ctx, repository); err == nil {
			t.Errorf("%s: LockedTags should fail", repository)
		}
	}
	if got := len(retry.waited()); got != 3 {
		t.Errorf("retried %d times in all, want no retries of final failures", got)
	}
}

// TestRetriesStopOnCancel: an interrupted run stops waiting to retry.
func TestRetriesStopOnCancel(t *testing.T) {
	f := newFakeACR()
	f.flaky["DELETE /v2/app/manifests/sha256:aaa"] = []int{503, 503}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupt := retryPolicy{maxRetries: 3, delay: time.Hour, maxDelay: time.Hour, sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		return registry.Sleep(ctx, d)
	}}
	b, err := newBackend(Options{Endpoint: srv.URL, PageSize: 10}, interrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: "sha256:aaa"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
	if got := f.requested(); len(got) != 1 {
		t.Errorf("requests = %v, want no retry after the interruption", got)
	}
}

// TestRetriesNetworkFailures: a response broken off by the network — garbled,
// or truncated — is retried too.
func TestRetriesNetworkFailures(t *testing.T) {
	f := newFakeACR()
	f.documents["sha256:aaa"] = `{"schemaVersion": 2}`
	f.manifests["app"] = []any{map[string]any{"digest": "sha256:aaa"}}
	broken := map[string]string{
		"/v2/app/manifests/sha256:aaa": "garbage\r\n\r\n",
		"/acr/v1/app/_manifests":       "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"manifests\": [",
	}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		response, breaking := broken[r.URL.Path]
		breaking = breaking && r.Header.Get("Authorization") != ""
		if breaking {
			delete(broken, r.URL.Path)
		}
		mu.Unlock()
		if !breaking {
			f.ServeHTTP(w, r)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = buffered.WriteString(response)
		_ = buffered.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	retry := &testRetry{}
	b, err := newBackend(Options{Endpoint: srv.URL, PageSize: 10}, retry.policy())
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := b.GetManifest(context.Background(), "app", "sha256:aaa"); err != nil || string(raw) != `{"schemaVersion": 2}` {
		t.Fatalf("GetManifest = %q, %v", raw, err)
	}
	listed := 0
	if err := b.ListManifests(context.Background(), "app", func(registry.Attributes) error { listed++; return nil }); err != nil || listed != 1 {
		t.Fatalf("ListManifests listed %d, %v", listed, err)
	}
	if got := retry.waited(); len(got) != 2 {
		t.Errorf("waited %v, want one retry of each", got)
	}
}

// TestPermanentFailuresAreFinal: a mistyped registry name or a certificate
// that does not verify retried every request for about 13 minutes.
func TestPermanentFailuresAreFinal(t *testing.T) {
	untrusted := httptest.NewTLSServer(newFakeACR())
	t.Cleanup(untrusted.Close)
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	closed := httptest.NewServer(newFakeACR())
	closed.Close()
	for _, tt := range []struct {
		name, endpoint string
		retried        bool
		// skip reports whether the environment keeps the failure from
		// showing, as a resolver without network access does.
		skip func(error) bool
	}{
		{name: "certificate signed by an unknown authority", endpoint: untrusted.URL},
		{name: "host that does not exist", endpoint: "https://acrprune.invalid", skip: func(err error) bool {
			dnsErr, ok := errors.AsType[*net.DNSError](err)
			return !ok || !dnsErr.IsNotFound
		}},
		{name: "connection refused", endpoint: closed.URL, retried: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			retry := &testRetry{}
			b, err := newBackend(Options{Endpoint: tt.endpoint, PageSize: 10}, retry.policy())
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.GetManifest(t.Context(), "app", "sha256:aaa")
			if err == nil {
				t.Fatal("the request should fail")
			}
			if tt.skip != nil && tt.skip(err) {
				t.Skipf("the environment reports another failure: %v", err)
			}
			if got, want := len(retry.waited()), map[bool]int{false: 0, true: 3}[tt.retried]; got != want {
				t.Errorf("retried %d times, want %d: %v", got, want, err)
			}
		})
	}
}

// TestTransient: which failures to get a response are worth retrying.
func TestTransient(t *testing.T) {
	syscallErr := func(op string, errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "https://myreg.azurecr.io/v2/", Err: &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}}
	}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", &url.Error{Op: "Get", Err: context.DeadlineExceeded}, true},
		{"connection refused", syscallErr("dial", syscall.ECONNREFUSED), true},
		{"connection reset", syscallErr("read", syscall.ECONNRESET), true},
		{"broken pipe", syscallErr("write", syscall.EPIPE), true},
		{"closed before responding", &url.Error{Op: "Get", Err: io.EOF}, true},
		{"truncated", fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), true},
		{"garbled", &url.Error{Op: "Get", Err: fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", errors.New(`malformed HTTP response "garbage"`))}, true},
		{"resolver timeout", &net.DNSError{Err: "i/o timeout", Name: "myreg.azurecr.io", IsTimeout: true}, true},
		{"resolver failure", &net.DNSError{Err: "server misbehaving", Name: "myreg.azurecr.io", IsTemporary: true}, true},
		{"no such host", &url.Error{Op: "Get", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "myreg.azurecr.io", IsNotFound: true}}}, false},
		{"unknown authority", &url.Error{Op: "Get", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		{"wrong host name", &url.Error{Op: "Get", Err: x509.HostnameError{Host: "myreg.azurecr.io", Certificate: &x509.Certificate{}}}, false},
		{"expired", &url.Error{Op: "Get", Err: x509.CertificateInvalidError{Reason: x509.Expired}}, false},
		{"TLS alert", &url.Error{Op: "Get", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}}, false},
		{"not TLS", &url.Error{Op: "Get", Err: errors.New("http: server gave HTTP response to HTTPS client")}, false},
		{"not retriable", fmt.Errorf("token: %w", nonRetriable{}), false},
		{"anything else", errors.New("unforeseen"), false},
	} {
		if got := registry.Transient(tt.err); got != tt.want {
			t.Errorf("%s: transient(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// nonRetriable is an error the SDK marks as not worth retrying.
type nonRetriable struct{}

func (nonRetriable) Error() string { return "failed" }
func (nonRetriable) NonRetriable() {}

func TestBackoff(t *testing.T) {
	var got []time.Duration
	var total time.Duration
	for retry := range defaultRetry.maxRetries {
		got = append(got, defaultRetry.backoff(retry))
		total += got[retry]
	}
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 180, 180, 180}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(got, want) {
		t.Errorf("default backoff = %v, want %v", got, want)
	}
	if total < 10*time.Minute {
		t.Errorf("retries give up after %v, want throttling ridden out for minutes", total)
	}
	huge := retryPolicy{delay: time.Hour, maxDelay: 1 << 62}
	for retry := range 64 {
		if d := huge.backoff(retry); d < time.Hour {
			t.Fatalf("backoff(%d) = %v overflowed", retry, d)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		header, value string
		want          time.Duration
	}{
		{"Retry-After", "30", 30 * time.Second},
		{"Retry-After", now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{"Retry-After", "soon", 0},
		{"Retry-After", "-1", 0},
		{"Retry-After-Ms", "1500", 1500 * time.Millisecond},
		{"X-Ms-Retry-After-Ms", "250", 250 * time.Millisecond},
		{"Retry-After", "99999999999999", time.Hour},
	} {
		resp := &http.Response{Header: http.Header{}}
		resp.Header.Set(tt.header, tt.value)
		if got := retryAfter(resp, now); got != tt.want {
			t.Errorf("%s: %s = %v, want %v", tt.header, tt.value, got, tt.want)
		}
	}
}

func TestCloud(t *testing.T) {
	for host, want := range map[string]string{
		"myreg.azurecr.cn": "https://login.chinacloudapi.cn/",
		"myreg.azurecr.us": "https://login.microsoftonline.us/",
		"myreg.azurecr.io": "",
		"myreg.azurecr.de": "",
	} {
		if got := Cloud(host).ActiveDirectoryAuthorityHost; got != want {
			t.Errorf("Cloud(%s) authority = %q, want %q", host, got, want)
		}
	}
}

// TestCredentialOptions: a sovereign login server selects its cloud's
// authority, unless AZURE_AUTHORITY_HOST names one.
func TestCredentialOptions(t *testing.T) {
	t.Setenv("AZURE_AUTHORITY_HOST", "")
	if got := CredentialOptions("myreg.azurecr.cn").Cloud.ActiveDirectoryAuthorityHost; got != "https://login.chinacloudapi.cn/" {
		t.Errorf("authority = %q, want Azure China's", got)
	}
	if got := CredentialOptions("myreg.azurecr.io").Cloud.ActiveDirectoryAuthorityHost; got != "" {
		t.Errorf("authority = %q, want the default", got)
	}
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.example/")
	if got := CredentialOptions("myreg.azurecr.cn").Cloud.ActiveDirectoryAuthorityHost; got != "" {
		t.Errorf("authority = %q, want AZURE_AUTHORITY_HOST left in charge", got)
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
	err := forEachPage(context.Background(), fakePager([]int{1, 2, 3}), nil, func(page int) error {
		got = append(got, page)
		return nil
	})
	if err != nil || !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("forEachPage = %v, %v", got, err)
	}

	// A paging error aborts the walk, classified for the caller.
	got = nil
	err = forEachPage(context.Background(), fakePager([]int{1, -1, 3}), nil, func(page int) error {
		got = append(got, page)
		return nil
	})
	if !registry.IsPermissionError(err) || len(got) != 1 {
		t.Errorf("paging error not propagated: %v, %v", got, err)
	}

	// An fn error aborts the walk too.
	boom := errors.New("boom")
	err = forEachPage(context.Background(), fakePager([]int{1, 2}), nil, func(int) error { return boom })
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
