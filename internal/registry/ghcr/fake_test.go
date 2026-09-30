package ghcr

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
)

const (
	testOwner = "acme"
	testToken = "ghp_test"
)

// fakeGitHub emulates, for one owner, the GitHub REST API's container package
// endpoints and ghcr.io's registry API with its token exchange.
type fakeGitHub struct {
	t    *testing.T
	url  string
	orgs bool // the owner is an organization

	// before, when set, sees every request first and may answer it itself,
	// returning true when it did.
	before func(w http.ResponseWriter, r *http.Request) bool
	// blobStorage, when set, is where blob downloads are redirected instead
	// of the fake itself.
	blobStorage string
	// scopes, when not nil, are the token's scopes, reported in
	// X-OAuth-Scopes as GitHub does for classic personal access tokens.
	scopes []string
	// denyDeletes answers every DELETE with 404, as GitHub answers a token
	// that may read a package but not delete from it.
	denyDeletes bool

	mu        sync.Mutex
	packages  map[string][]*fakeVersion // name -> versions, newest first
	documents map[string][]byte         // digest -> manifest document
	blobs     map[string][]byte         // digest -> blob
	nextID    int64
	issued    map[string]string // registry token -> scope
	tokens    int               // token exchanges
	requests  []string          // "METHOD path" plus " (anonymous)" for unauthenticated registry requests
}

type fakeVersion struct {
	id      int64
	digest  string
	tags    []string
	updated time.Time
}

// newFakeGitHub starts a fake for an organization or user named testOwner and
// returns it with a backend talking to it. The backend fails the test if it
// retries anything; see recordSleeps.
func newFakeGitHub(t *testing.T, orgs bool) (*fakeGitHub, *Backend) {
	t.Helper()
	f := &fakeGitHub{
		t:         t,
		orgs:      orgs,
		packages:  map[string][]*fakeVersion{},
		documents: map[string][]byte{},
		blobs:     map[string][]byte{},
		nextID:    1000,
		issued:    map[string]string{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f, f.backend(t, 100)
}

// backend connects a new backend to the fake.
func (f *fakeGitHub) backend(t *testing.T, pageSize int) *Backend {
	t.Helper()
	b, err := New(context.Background(), Options{
		Owner:       testOwner,
		Token:       testToken,
		PageSize:    pageSize,
		APIURL:      f.url,
		RegistryURL: f.url,
	})
	if err != nil {
		t.Fatal(err)
	}
	b.sleep = func(context.Context, time.Duration) error {
		t.Fatal("unexpected retry")
		return nil
	}
	return b
}

// recordSleeps makes b's retries return at once, recording their delays, and
// gives b a clock that only its sleeps advance.
func recordSleeps(b *Backend) *[]time.Duration {
	var mu sync.Mutex
	delays := new([]time.Duration)
	clock := time.Now()
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	b.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		*delays = append(*delays, d)
		clock = clock.Add(d)
		return nil
	}
	return delays
}

// push adds a package version holding doc and returns its digest.
func (f *fakeGitHub) push(pkg string, doc any, updated time.Time, tags ...string) string {
	raw, ok := doc.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(doc); err != nil {
			f.t.Fatal(err)
		}
	}
	digest := godigest.FromBytes(raw).String()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.documents[digest] = raw
	f.packages[pkg] = append([]*fakeVersion{{id: f.nextID, digest: digest, tags: tags, updated: updated}}, f.packages[pkg]...)
	return digest
}

// pushUnserved adds a package version whose manifest ghcr.io cannot serve,
// and returns its digest.
func (f *fakeGitHub) pushUnserved(pkg string, updated time.Time, tags ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	digest := godigest.FromString(fmt.Sprintf("unserved %d", f.nextID)).String()
	f.packages[pkg] = append([]*fakeVersion{{id: f.nextID, digest: digest, tags: tags, updated: updated}}, f.packages[pkg]...)
	return digest
}

// pushBlob stores a blob and returns its digest.
func (f *fakeGitHub) pushBlob(content []byte) string {
	digest := godigest.FromBytes(content).String()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[digest] = content
	return digest
}

// remaining returns the digests left in a package, sorted, or nil when the
// package is gone.
func (f *fakeGitHub) remaining(pkg string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var digests []string
	for _, v := range f.packages[pkg] {
		digests = append(digests, v.digest)
	}
	slices.Sort(digests)
	return digests
}

// hasPackage reports whether the package exists.
func (f *fakeGitHub) hasPackage(pkg string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.packages[pkg]
	return ok
}

// versionID returns the id of the version holding digest.
func (f *fakeGitHub) versionID(pkg, digest string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.packages[pkg] {
		if v.digest == digest {
			return strconv.FormatInt(v.id, 10)
		}
	}
	f.t.Fatalf("no version %s in %s", digest, pkg)
	return ""
}

// expireTokens invalidates every registry token issued so far.
func (f *fakeGitHub) expireTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.issued)
}

func (f *fakeGitHub) tokenExchanges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

// requested returns the requests served so far matching prefix.
func (f *fakeGitHub) requested(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			matching = append(matching, r)
		}
	}
	return matching
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if f.before != nil && f.before(w, r) {
		return
	}
	switch {
	case path == "/token":
		f.serveToken(w, r)
	case strings.HasPrefix(path, "/v2/"):
		f.serveRegistry(w, r, strings.TrimPrefix(path, "/v2/"))
	case strings.HasPrefix(path, "/cdn/"):
		f.log(r, "")
		f.serveContent(w, f.blobs, strings.TrimPrefix(path, "/cdn/"))
	default:
		f.serveAPI(w, r, path)
	}
}

func (f *fakeGitHub) log(r *http.Request, suffix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath()+suffix)
}

// serveAPI serves the REST API.
func (f *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request, path string) {
	f.log(r, "")
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		apiError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("User-Agent") == "" {
		apiError(w, http.StatusBadRequest, "missing API version or user agent")
		return
	}
	if f.scopes != nil {
		w.Header().Set("X-OAuth-Scopes", strings.Join(f.scopes, ", "))
	}
	if path == "/users/"+testOwner && r.Method == http.MethodGet {
		kind := "User"
		if f.orgs {
			kind = "Organization"
		}
		writeJSON(w, http.StatusOK, map[string]string{"login": testOwner, "type": kind})
		return
	}
	namespace := "users"
	if f.orgs {
		namespace = "orgs"
	}
	rest, ok := strings.CutPrefix(path, "/"+namespace+"/"+testOwner+"/packages")
	if !ok {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if rest == "" && r.Method == http.MethodGet {
		if r.URL.Query().Get("package_type") != "container" {
			apiError(w, http.StatusBadRequest, "package_type is required")
			return
		}
		var names []any
		for _, name := range slices.Sorted(maps.Keys(f.packages)) {
			names = append(names, map[string]any{"name": name, "package_type": "container"})
		}
		f.page(w, r, names)
		return
	}

	segments := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(segments) < 2 || segments[0] != "container" {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	pkg, err := url.PathUnescape(segments[1])
	versions, exists := f.packages[pkg]
	if err != nil || !exists || f.denyDeletes && r.Method == http.MethodDelete {
		apiError(w, http.StatusNotFound, "Package not found.")
		return
	}
	switch {
	case len(segments) == 2 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"name": pkg, "package_type": "container", "version_count": len(versions)})
	case len(segments) == 4 && segments[2] == "versions" && r.Method == http.MethodGet:
		i := slices.IndexFunc(versions, func(v *fakeVersion) bool { return strconv.FormatInt(v.id, 10) == segments[3] })
		if i < 0 {
			apiError(w, http.StatusNotFound, "Package version not found.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": versions[i].id, "name": versions[i].digest})
	case len(segments) == 2 && r.Method == http.MethodDelete:
		delete(f.packages, pkg)
		w.WriteHeader(http.StatusNoContent)
	case len(segments) == 3 && segments[2] == "versions" && r.Method == http.MethodGet:
		var page []any
		for _, v := range versions {
			page = append(page, map[string]any{
				"id":         v.id,
				"name":       v.digest,
				"created_at": v.updated.Add(-time.Hour).Format(time.RFC3339),
				"updated_at": v.updated.Format(time.RFC3339),
				"metadata": map[string]any{
					"package_type": "container",
					"container":    map[string]any{"tags": append([]string{}, v.tags...)},
				},
			})
		}
		f.page(w, r, page)
	case len(segments) == 4 && segments[2] == "versions" && r.Method == http.MethodDelete:
		i := slices.IndexFunc(versions, func(v *fakeVersion) bool { return strconv.FormatInt(v.id, 10) == segments[3] })
		if i < 0 {
			apiError(w, http.StatusNotFound, "Package version not found.")
			return
		}
		tagged := 0
		for _, v := range versions {
			if len(v.tags) > 0 {
				tagged++
			}
		}
		if len(versions[i].tags) > 0 && tagged == 1 {
			apiError(w, http.StatusBadRequest, "You cannot delete the last tagged version of a package. You must delete the package instead.")
			return
		}
		f.packages[pkg] = slices.Delete(versions, i, i+1)
		w.WriteHeader(http.StatusNoContent)
	default:
		apiError(w, http.StatusNotFound, "Not Found")
	}
}

// page writes one page of a listing, as selected by the page and per_page
// query parameters, with the Link header GitHub sends.
func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, items []any) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		apiError(w, http.StatusBadRequest, "per_page must be between 1 and 100")
		return
	}
	pageNumber, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageNumber = max(pageNumber, 1)
	start := min((pageNumber-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	if end < len(items) {
		next := *r.URL
		query := next.Query()
		query.Set("page", strconv.Itoa(pageNumber+1))
		next.RawQuery = query.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next", <%s%s>; rel="last"`, f.url, next.RequestURI(), f.url, next.RequestURI()))
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, items[start:end])
}

// serveToken exchanges the GitHub token, presented as a basic auth password,
// for a registry token.
func (f *fakeGitHub) serveToken(w http.ResponseWriter, r *http.Request) {
	f.log(r, "")
	user, password, ok := r.BasicAuth()
	if !ok || user == "" || password != testToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []map[string]string{{"code": "UNAUTHORIZED", "message": "authentication required"}}})
		return
	}
	scope := r.URL.Query().Get("scope")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens++
	token := fmt.Sprintf("registry-token-%d", f.tokens)
	f.issued[token] = scope
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// serveRegistry serves manifests and blobs below /v2/.
func (f *fakeGitHub) serveRegistry(w http.ResponseWriter, r *http.Request, path string) {
	segments := strings.Split(path, "/")
	if len(segments) < 4 || segments[0] != testOwner {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	repository := strings.Join(segments[:len(segments)-2], "/")
	kind, digest := segments[len(segments)-2], segments[len(segments)-1]
	scope := "repository:" + repository + ":pull"

	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	authorized := f.issued[token] == scope
	f.mu.Unlock()
	if !authorized {
		f.log(r, " (anonymous)")
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="%s"`, f.url, scope))
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	f.log(r, "")

	switch kind {
	case "manifests":
		f.mu.Lock()
		pkg := strings.TrimPrefix(repository, testOwner+"/")
		listed := slices.ContainsFunc(f.packages[pkg], func(v *fakeVersion) bool { return v.digest == digest })
		f.mu.Unlock()
		if !listed {
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		f.serveContent(w, f.documents, digest)
	case "blobs":
		// ghcr.io redirects blob downloads to signed storage URLs.
		storage := f.url
		if f.blobStorage != "" {
			storage = f.blobStorage
		}
		http.Redirect(w, r, storage+"/cdn/"+digest+"?signature=secret", http.StatusTemporaryRedirect)
	default:
		registryError(w, http.StatusNotFound, "UNSUPPORTED")
	}
}

func (f *fakeGitHub) serveContent(w http.ResponseWriter, contents map[string][]byte, digest string) {
	f.mu.Lock()
	content, ok := contents[digest]
	f.mu.Unlock()
	if !ok {
		registryError(w, http.StatusNotFound, "BLOB_UNKNOWN")
		return
	}
	_, _ = w.Write(content)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message, "documentation_url": "https://docs.github.com/rest"})
}

func registryError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"code": code, "message": strings.ToLower(code)}}})
}
