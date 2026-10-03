package ghcr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
)

var ctx = context.Background()

func manifestDoc(name string) map[string]any {
	return map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + strings.Repeat("c", 64), "size": 2},
		"annotations":   map[string]string{"name": name},
	}
}

func listAll(t *testing.T, b *Backend, repository string) []registry.Attributes {
	t.Helper()
	var listed []registry.Attributes
	err := b.ListManifests(ctx, repository, func(attrs registry.Attributes) error {
		listed = append(listed, attrs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

// TestNewResolvesNamespace: organizations and users keep their packages under
// different REST paths.
func TestNewResolvesNamespace(t *testing.T) {
	for _, orgs := range []bool{true, false} {
		f, b := newFakeGitHub(t, orgs)
		f.push("app", manifestDoc("a"), time.Now(), "v1")
		names, err := b.ListRepositories(ctx)
		if err != nil || !slices.Equal(names, []string{"app"}) {
			t.Errorf("orgs=%v: ListRepositories = %v, %v", orgs, names, err)
		}
		want := "/users/" + testOwner + "/packages"
		if orgs {
			want = "/orgs/" + testOwner + "/packages"
		}
		if !strings.HasSuffix(b.packages, want) {
			t.Errorf("orgs=%v: packages URL %s, want it to end in %s", orgs, b.packages, want)
		}
	}
}

func TestNewErrors(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	valid := Options{Owner: testOwner, Token: testToken, PageSize: 10, APIURL: f.url, RegistryURL: f.url}

	tests := []struct {
		name   string
		modify func(*Options)
		want   string
	}{
		{"no token", func(o *Options) { o.Token = "" }, "token is required"},
		{"uppercase owner", func(o *Options) { o.Owner = "Acme" }, "invalid GHCR owner"},
		{"owner with a path", func(o *Options) { o.Owner = "acme/app" }, "invalid GHCR owner"},
		{"zero page size", func(o *Options) { o.PageSize = 0 }, "page size"},
		{"bad API URL", func(o *Options) { o.APIURL = "::" }, "invalid service URL"},
		{"bad registry URL", func(o *Options) { o.RegistryURL = "no-scheme" }, "invalid service URL"},
		{"unknown owner", func(o *Options) { o.Owner = "nobody" }, "no GitHub user or organization named"},
		{"bad token", func(o *Options) { o.Token = "wrong" }, "Bad credentials"},
	}
	for _, tt := range tests {
		opts := valid
		tt.modify(&opts)
		if _, err := New(ctx, opts); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
	if _, err := New(ctx, valid); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
}

// TestNewChecksScopes: GitHub reports the scopes of classic and OAuth tokens,
// so one that cannot do the job fails before any scan; other tokens report
// none and are not checked.
func TestNewChecksScopes(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		needDelete bool
		want       string // the error, or "" for none
	}{
		{"fine-grained token", nil, true, ""},
		{"read", []string{"repo", "read:packages"}, false, ""},
		{"write includes read", []string{"write:packages"}, false, ""},
		{"read and delete", []string{"read:packages", "delete:packages"}, true, ""},
		{"no packages scope", []string{"repo", "workflow"}, false, "lacks the read:packages scope (it has: repo, workflow)"},
		{"no scopes", []string{}, false, "lacks the read:packages scope (it has: none)"},
		{"no delete", []string{"read:packages"}, true, "lacks the delete:packages scope"},
		{"neither", []string{"gist"}, true, "lacks the read:packages and delete:packages scopes"},
	}
	for _, tt := range tests {
		f, _ := newFakeGitHub(t, true)
		f.scopes = tt.scopes
		_, err := New(ctx, Options{Owner: testOwner, Token: testToken, PageSize: 10, APIURL: f.url, RegistryURL: f.url, NeedDelete: tt.needDelete})
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "gh auth refresh --scopes read:packages,delete:packages")):
			t.Errorf("%s: error = %v, want it to say %q and how to fix it", tt.name, err, tt.want)
		}
	}
}

// TestNewDecodeFailure: an account lookup that is not JSON fails New.
func TestNewDecodeFailure(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte("<html>"))
		return true
	}
	if _, err := New(ctx, Options{Owner: testOwner, Token: testToken, PageSize: 10, APIURL: f.url, RegistryURL: f.url}); err == nil || !strings.Contains(err.Error(), "failed to decode GitHub account") {
		t.Errorf("error = %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	b, err := New(ctx, Options{Owner: testOwner, Token: testToken, PageSize: 500, APIURL: f.url, RegistryURL: f.url})
	if err != nil {
		t.Fatal(err)
	}
	if b.pageSize != maxPageSize {
		t.Errorf("page size = %d, want it capped at %d", b.pageSize, maxPageSize)
	}
	if b.username != defaultUsername || b.client == nil || b.logger == nil {
		t.Errorf("defaults not applied: username %q, client %v, logger %v", b.username, b.client, b.logger)
	}
	if !b.ProtectsLastTag() {
		t.Error("GHCR refuses to delete the last tagged version")
	}
}

func TestListRepositoriesPaged(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	for _, name := range []string{"app", "team/api", "tools"} {
		f.push(name, manifestDoc(name), time.Now(), "v1")
	}
	b := f.backend(t, 2)

	names, err := b.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"app", "team/api", "tools"}) {
		t.Errorf("ListRepositories = %v", names)
	}
	if pages := f.requested("GET /orgs/" + testOwner + "/packages"); len(pages) != 2 {
		t.Errorf("listing took %d requests, want 2 pages: %v", len(pages), pages)
	}
}

func TestListRepositoriesPermission(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/packages") {
			apiError(w, http.StatusForbidden, "You need at least read:packages scope to list packages.")
			return true
		}
		return false
	}
	_, err := b.ListRepositories(ctx)
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "read:packages") {
		t.Errorf("error = %v, want a permission error naming the scope", err)
	}
}

func TestListManifests(t *testing.T) {
	f, _ := newFakeGitHub(t, true)
	updated := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	older := f.push("team/app", manifestDoc("older"), updated.Add(-time.Hour))
	newer := f.push("team/app", manifestDoc("newer"), updated, "v2", "latest")
	b := f.backend(t, 1)

	listed := listAll(t, b, "team/app")
	if len(listed) != 2 {
		t.Fatalf("listed %d versions, want 2 across two pages", len(listed))
	}
	first := listed[0]
	if first.Digest != newer || !slices.Equal(first.Tags, []string{"v2", "latest"}) || !first.LastUpdated.Equal(updated) || first.ID != f.versionID("team/app", newer) {
		t.Errorf("attributes = %+v", first)
	}
	if first.Architecture != "" || first.OS != "" || first.Locked {
		t.Errorf("GHCR reports no platform or locks: %+v", first)
	}
	if second := listed[1]; second.Digest != older || len(second.Tags) != 0 {
		t.Errorf("untagged version attributes = %+v", second)
	}
	// The package name is one path segment in the REST API.
	if got := f.requested("GET /orgs/" + testOwner + "/packages/container/team%2Fapp/versions"); len(got) != 2 {
		t.Errorf("version listing requests = %v", got)
	}

	// GitHub hides private packages a token may not read the same way.
	if err := b.ListManifests(ctx, "gone", func(registry.Attributes) error { return nil }); !registry.IsNotFound(err) || !strings.Contains(err.Error(), "not visible to this token") {
		t.Errorf("missing package error = %v, want not found, naming the possible lack of access", err)
	}
	boom := errors.New("boom")
	if err := b.ListManifests(ctx, "team/app", func(registry.Attributes) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("fn error = %v, want %v", err, boom)
	}
	if err := b.ListManifests(ctx, "team/../app", func(registry.Attributes) error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid repository name") {
		t.Errorf("unsafe repository name: error = %v", err)
	}
}

// TestVersionAttributesFallBackToCreation: a version without an update time
// counts from its creation.
func TestVersionAttributesFallBackToCreation(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	attrs := packageVersion{ID: 7, Name: "sha256:abc", CreatedAt: created}.attributes()
	if !attrs.LastUpdated.Equal(created) || attrs.ID != "7" || attrs.Digest != "sha256:abc" {
		t.Errorf("attributes = %+v", attrs)
	}
}

func TestGetManifestAuthenticates(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	first := f.push("app", manifestDoc("first"), time.Now(), "v1")
	second := f.push("app", manifestDoc("second"), time.Now(), "v2")
	other := f.push("team/api", manifestDoc("other"), time.Now(), "v1")

	for _, digest := range []string{first, second} {
		raw, err := b.GetManifest(ctx, "app", digest)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"schemaVersion":2`) {
			t.Errorf("GetManifest returned %s", raw)
		}
	}
	// The first request learns the token endpoint from the challenge; the
	// token is then reused for the repository.
	if got := f.tokenExchanges(); got != 1 {
		t.Errorf("token exchanges = %d, want 1 for one repository", got)
	}
	if anonymous := f.requested("GET /v2/"); len(anonymous) != 3 || !strings.HasSuffix(anonymous[0], "(anonymous)") {
		t.Errorf("registry requests = %v, want one anonymous probe then two authenticated", anonymous)
	}

	// Another repository needs another token, fetched before the request
	// now that the endpoint is known.
	if _, err := b.GetManifest(ctx, "team/api", other); err != nil {
		t.Fatal(err)
	}
	if got := f.tokenExchanges(); got != 2 {
		t.Errorf("token exchanges = %d, want 2 for two repositories", got)
	}
	for _, r := range f.requested("GET /v2/" + testOwner + "/team/api") {
		if strings.HasSuffix(r, "(anonymous)") {
			t.Errorf("request %s went out without the known token", r)
		}
	}
}

// TestGetManifestRenewsRejectedToken: registry tokens expire; a rejected one
// is renewed once.
func TestGetManifestRenewsRejectedToken(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
	if _, err := b.GetManifest(ctx, "app", digest); err != nil {
		t.Fatal(err)
	}
	f.expireTokens()
	if _, err := b.GetManifest(ctx, "app", digest); err != nil {
		t.Fatalf("an expired token should be renewed: %v", err)
	}
	if got := f.tokenExchanges(); got != 2 {
		t.Errorf("token exchanges = %d, want 2", got)
	}
}

func TestGetManifestRejectedCredentials(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
	b.token = "revoked"
	if _, err := b.GetManifest(ctx, "app", digest); !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "registry token") {
		t.Errorf("error = %v, want a permission error from the token exchange", err)
	}
}

func TestGetManifestMissing(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	_, err := b.GetManifest(ctx, "app", "sha256:"+strings.Repeat("0", 64))
	if !registry.IsNotFound(err) || !strings.Contains(err.Error(), "MANIFEST_UNKNOWN") {
		t.Errorf("error = %v, want not found with the registry's error code", err)
	}
}

// TestRegistryRejectsUnsafeNames: repository names and digests go into
// registry URL paths verbatim.
func TestRegistryRejectsUnsafeNames(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	valid := "sha256:" + strings.Repeat("0", 64)
	for _, tc := range []struct{ repository, digest string }{
		{"../escape", valid},
		{"App", valid},
		{"app/", valid},
		{"app", "sha256:../../x"},
		{"app", "latest"},
	} {
		if _, err := b.GetManifest(ctx, tc.repository, tc.digest); err == nil {
			t.Errorf("GetManifest(%q, %q) should be refused", tc.repository, tc.digest)
		}
	}
	if got := f.requested("GET /v2/"); len(got) != 0 {
		t.Errorf("refused names still made requests: %v", got)
	}
}

// TestGetBlobFollowsRedirect: ghcr.io serves blobs from signed storage URLs.
func TestGetBlobFollowsRedirect(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	digest := f.pushBlob([]byte(`{"architecture":"arm64","os":"linux"}`))

	content, err := b.GetBlob(ctx, "app", digest)
	if err != nil || string(content) != `{"architecture":"arm64","os":"linux"}` {
		t.Fatalf("GetBlob = %q, %v", content, err)
	}
	if got := f.requested("GET /cdn/"); len(got) != 1 {
		t.Errorf("blob storage requests = %v", got)
	}

	_, err = b.GetBlob(ctx, "app", "sha256:"+strings.Repeat("0", 64))
	if !registry.IsNotFound(err) {
		t.Errorf("missing blob error = %v, want not found", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error %q leaks the storage URL's signature", err)
	}
}

func TestDeleteManifest(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	keep := f.push("team/app", manifestDoc("keep"), time.Now(), "v1")
	drop := f.push("team/app", manifestDoc("drop"), time.Now(), "v2")
	id := f.versionID("team/app", drop)
	m := &registry.Manifest{Repository: "team/app", Attributes: registry.Attributes{Digest: drop, ID: id}}

	if err := b.DeleteManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := f.remaining("team/app"); !slices.Equal(got, []string{keep}) {
		t.Errorf("remaining = %v, want only %s", got, keep)
	}
	if got := f.requested("DELETE /orgs/" + testOwner + "/packages/container/team%2Fapp/versions/" + id); len(got) != 1 {
		t.Errorf("delete requests = %v", f.requested("DELETE"))
	}

	// Deleting what is already gone succeeds, as it does on ACR, once a GET
	// confirms it is gone.
	if err := b.DeleteManifest(ctx, m); err != nil {
		t.Errorf("deleting a deleted version: %v", err)
	}
	if got := f.requested("GET /orgs/" + testOwner + "/packages/container/team%2Fapp/versions/" + id); len(got) != 1 {
		t.Errorf("confirming requests = %v", got)
	}

	for _, bad := range []string{"", "0", "12/../34", "-1"} {
		m.ID = bad
		if err := b.DeleteManifest(ctx, m); err == nil || !strings.Contains(err.Error(), "package version id") {
			t.Errorf("ID %q: error = %v, want a missing-id error", bad, err)
		}
	}
	m.ID, m.Repository = id, "../escape"
	if err := b.DeleteManifest(ctx, m); err == nil || !strings.Contains(err.Error(), "invalid repository name") {
		t.Errorf("unsafe repository name: error = %v", err)
	}
	if err := b.DeleteRepository(ctx, "Team/App"); err == nil || !strings.Contains(err.Error(), "invalid repository name") {
		t.Errorf("unsafe repository name: error = %v", err)
	}
	if got := f.requested("DELETE"); len(got) != 2 {
		t.Errorf("refused names still made requests: %v", got)
	}
}

// TestDeleteDeniedAsNotFound: GitHub answers a DELETE the token may not make
// with 404, as if the resource were gone. One that still exists is reported
// as a permission error, so the package counts as denied rather than pruned.
func TestDeleteDeniedAsNotFound(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("keep"), time.Now(), "v1")
	drop := f.push("app", manifestDoc("drop"), time.Now())
	want := f.remaining("app")
	f.denyDeletes = true

	err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: drop, ID: f.versionID("app", drop)}})
	if !registry.IsPermissionError(err) || registry.IsNotFound(err) || !strings.Contains(err.Error(), "delete:packages") {
		t.Errorf("DeleteManifest error = %v, want a permission error naming the scope", err)
	}
	err = b.DeleteRepository(ctx, "app")
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "still exists") {
		t.Errorf("DeleteRepository error = %v, want a permission error", err)
	}
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

// TestDeleteUnconfirmed: a DELETE answered with 404 whose resource cannot be
// looked up is not taken for deleted.
func TestDeleteUnconfirmed(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	drop := f.push("app", manifestDoc("drop"), time.Now())
	id := f.versionID("app", drop)
	recordSleeps(b)
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			apiError(w, http.StatusNotFound, "Package not found.")
		} else {
			serverError(w)
		}
		return true
	}
	err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: drop, ID: id}})
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusBadGateway || !strings.Contains(err.Error(), "checking whether it still exists failed") {
		t.Errorf("error = %v, want the failed check", err)
	}
}

// TestDeleteRefusesRewritingRedirect: Go follows a DELETE redirected with 301
// as a GET, whose success must not pass for a deletion. The refusal is final,
// not retried.
func TestDeleteRefusesRewritingRedirect(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("keep"), time.Now(), "v1")
	drop := f.push("app", manifestDoc("drop"), time.Now())
	want := f.remaining("app")
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodDelete {
			return false
		}
		http.Redirect(w, r, "/users/"+testOwner, http.StatusMovedPermanently)
		return true
	}
	err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: drop, ID: f.versionID("app", drop)}})
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect that turns DELETE into GET") {
		t.Errorf("error = %v, want the refused redirect", err)
	}
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	if got := f.requested("GET /users/"); len(got) != 1 {
		t.Errorf("the redirect was followed: %v", got)
	}
}

// TestListingDecodeFailures: a listing page that is not JSON fails the
// listing.
func TestListingDecodeFailures(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(`{"not": "a list"}`))
		return true
	}
	if _, err := b.ListRepositories(ctx); err == nil || !strings.Contains(err.Error(), "failed to decode packages") {
		t.Errorf("ListRepositories error = %v", err)
	}
	if err := b.ListManifests(ctx, "app", func(registry.Attributes) error { return nil }); err == nil || !strings.Contains(err.Error(), "failed to decode package versions") {
		t.Errorf("ListManifests error = %v", err)
	}
}

func TestListingRejectsNullAndTrailingContent(t *testing.T) {
	for _, body := range []string{"null", "[] []", "[] trailing", "[null]"} {
		t.Run(body, func(t *testing.T) {
			f, b := newFakeGitHub(t, true)
			f.before = func(w http.ResponseWriter, _ *http.Request) bool {
				_, _ = io.WriteString(w, body)
				return true
			}
			if _, err := b.ListRepositories(t.Context()); err == nil {
				t.Fatal("malformed package listing accepted")
			}
			if err := b.ListManifests(t.Context(), "app", func(registry.Attributes) error { return nil }); err == nil {
				t.Fatal("malformed version listing accepted")
			}
		})
	}
}

func TestVersionListingRequiresIdentityAndTagInventory(t *testing.T) {
	for _, body := range []string{
		`[{"id":0,"name":"sha256:abc","metadata":{"container":{"tags":[]}}}]`,
		`[{"id":-1,"name":"sha256:abc","metadata":{"container":{"tags":[]}}}]`,
		`[{"id":1,"name":"sha256:abc"}]`,
		`[{"id":1,"name":"sha256:abc","metadata":{"container":{}}}]`,
		`[{"id":1,"name":"sha256:abc","metadata":{"container":{"tags":null}}}]`,
	} {
		t.Run(body, func(t *testing.T) {
			f, b := newFakeGitHub(t, true)
			f.before = func(w http.ResponseWriter, _ *http.Request) bool {
				_, _ = io.WriteString(w, body)
				return true
			}
			listed := false
			if err := b.ListManifests(t.Context(), "app", func(registry.Attributes) error { listed = true; return nil }); err == nil || listed {
				t.Fatal("incomplete metadata reached retention rules", err)
			}
		})
	}
}

// TestListingStaysOnOrigin: the next page of a live listing is not fetched
// from another host, which would receive the token.
func TestListingStaysOnOrigin(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	var foreign atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer elsewhere.Close()
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/packages") {
			w.Header().Set("Link", "<"+elsewhere.URL+"/orgs/acme/packages?page=2>; rel=\"next\"")
		}
		return false
	}
	if _, err := b.ListRepositories(ctx); err == nil || !strings.Contains(err.Error(), "refusing to follow the next page link") {
		t.Errorf("error = %v", err)
	}
	if foreign.Load() != 0 {
		t.Error("the foreign host was asked for the next page")
	}
}

// TestDeleteLastTaggedVersion: GHCR's refusal surfaces as a plain response
// error, not as missing permission.
func TestDeleteLastTaggedVersion(t *testing.T) {
	f, b := newFakeGitHub(t, false)
	digest := f.push("app", manifestDoc("only"), time.Now(), "v1")
	f.push("app", manifestDoc("untagged"), time.Now())

	err := b.DeleteManifest(ctx, &registry.Manifest{Repository: "app", Attributes: registry.Attributes{Digest: digest, ID: f.versionID("app", digest)}})
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusBadRequest || registry.IsPermissionError(err) {
		t.Fatalf("error = %v, want a 400 response error", err)
	}
	if !strings.Contains(err.Error(), "last tagged version") {
		t.Errorf("error %q should carry GitHub's message", err)
	}
}

func TestDeleteRepository(t *testing.T) {
	f, b := newFakeGitHub(t, false)
	f.push("team/app", manifestDoc("a"), time.Now(), "v1")

	if err := b.DeleteRepository(ctx, "team/app"); err != nil {
		t.Fatal(err)
	}
	if f.hasPackage("team/app") {
		t.Error("the package should be gone")
	}
	if got := f.requested("DELETE /users/" + testOwner + "/packages/container/team%2Fapp"); len(got) != 1 {
		t.Errorf("delete requests = %v", f.requested("DELETE"))
	}
	if err := b.DeleteRepository(ctx, "team/app"); err != nil {
		t.Errorf("deleting a deleted package: %v", err)
	}
}
