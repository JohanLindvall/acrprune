package registry_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
)

func newRegistry(t *testing.T, backend registry.Backend, cache *registry.Cache) *registry.Registry {
	t.Helper()
	reg, err := registry.New(backend, slog.New(slog.DiscardHandler), 4, cache)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// plain hides every optional interface of a backend.
func plain(b registry.Backend) registry.Backend {
	return struct{ registry.Backend }{b}
}

func fetch(t *testing.T, reg *registry.Registry, repository string, opts registry.FetchOptions) map[string]*registry.Manifest {
	t.Helper()
	contents, found, err := reg.FetchRepositoryManifests(context.Background(), repository, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("repository %s not found", repository)
	}
	if len(contents.Missing) != 0 {
		t.Fatalf("repository %s has missing manifests: %+v", repository, contents.Missing)
	}
	return contents.Manifests
}

// multiArch stores a multi-platform image in fake's "app" repository: an
// index tagged v1 over an amd64 and an arm64 image, and returns the digests.
func multiArch(fake *registrytest.Backend, updated time.Time) (index, amd, arm string) {
	amd = fake.Add("app", registrytest.Image("amd"), registry.Attributes{LastUpdated: updated})
	arm = fake.Add("app", registrytest.Image("arm"), registry.Attributes{LastUpdated: updated})
	index = fake.Add("app", registrytest.Index(
		registrytest.Child(amd, "linux", "amd64"),
		registrytest.Child(arm, "linux", "arm64"),
	), registry.Attributes{Tags: []string{"v1"}, LastUpdated: updated, ID: "42"})
	return index, amd, arm
}

func TestFetchRepositoryManifests(t *testing.T) {
	fake := registrytest.New()
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	index, amd, arm := multiArch(fake, updated)
	solo := fake.Add("app", registrytest.Image("solo"), registry.Attributes{Tags: []string{"solo"}})

	manifests := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})
	if len(manifests) != 4 {
		t.Fatalf("fetched %d manifests, want 4", len(manifests))
	}

	idx := manifests[index]
	if idx.Repository != "app" || idx.Digest != index || idx.ID != "42" || !idx.LastUpdated.Equal(updated) || !slices.Equal(idx.Tags, []string{"v1"}) {
		t.Errorf("index attributes not attached: %+v", idx.Attributes)
	}
	if len(idx.Manifests) != 2 || idx.Size == 0 {
		t.Errorf("index document not parsed: %d children, size %d", len(idx.Manifests), idx.Size)
	}
	if strings.Join(idx.Architectures(), ",") != "amd64,arm64" {
		t.Errorf("index architectures = %v", idx.Architectures())
	}

	// A listing without platforms (GHCR) learns the children's from the
	// index referencing them, without downloading anything more.
	if m := manifests[amd]; m.Architecture != "amd64" || m.OS != "linux" {
		t.Errorf("amd64 child platform = %s/%s", m.OS, m.Architecture)
	}
	if m := manifests[arm]; m.Architecture != "arm64" {
		t.Errorf("arm64 child platform = %s/%s", m.OS, m.Architecture)
	}
	if m := manifests[solo]; m.Architecture != "" || m.OS != "" {
		t.Errorf("standalone image platform = %s/%s, want unknown unless asked for", m.OS, m.Architecture)
	}
	if fake.Calls("GetBlob") != 0 {
		t.Errorf("configs downloaded %d times without Platforms", fake.Calls("GetBlob"))
	}
}

// TestFetchKeepsReportedPlatforms: a platform the listing reports (ACR) wins
// over the index's descriptor, and is never looked up.
func TestFetchKeepsReportedPlatforms(t *testing.T) {
	fake := registrytest.New()
	child := fake.Add("app", registrytest.Image("child"), registry.Attributes{Architecture: "arm64", OS: "linux"})
	fake.Add("app", registrytest.Index(registrytest.Child(child, "windows", "amd64")), registry.Attributes{Tags: []string{"v1"}})

	manifests := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{Platforms: true})
	if m := manifests[child]; m.Architecture != "arm64" || m.OS != "linux" {
		t.Errorf("reported platform overridden: %s/%s", m.OS, m.Architecture)
	}
	if fake.Calls("GetBlob") != 0 {
		t.Error("a reported platform should not be looked up")
	}
}

func TestFetchPlatformsFromConfig(t *testing.T) {
	fake := registrytest.New()
	multiArch(fake, time.Now())
	config := fake.PutConfig("arm64", "linux")
	var images []string
	for _, name := range []string{"one", "two"} { // built alike: one shared config
		image := registrytest.Image(name)
		image.Config = &config
		images = append(images, fake.Add("app", image, registry.Attributes{Tags: []string{name}}))
	}
	broken := fake.Add("app", registrytest.Image("broken"), registry.Attributes{Tags: []string{"broken"}}) // config blob missing
	artifact := registrytest.Image("signature")
	artifact.Config = &v1.Descriptor{MediaType: v1.MediaTypeEmptyJSON, Digest: v1.DescriptorEmptyJSON.Digest, Size: 2}
	sig := fake.Add("app", artifact, registry.Attributes{})

	manifests := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{Platforms: true})
	for _, digest := range images {
		if m := manifests[digest]; m.Architecture != "arm64" || m.OS != "linux" {
			t.Errorf("platform from config = %s/%s, want linux/arm64", m.OS, m.Architecture)
		}
	}
	if m := manifests[broken]; m.Architecture != "" {
		t.Errorf("an image whose config is missing should have no platform, got %s", m.Architecture)
	}
	if m := manifests[sig]; m.Architecture != "" {
		t.Errorf("an artifact should have no platform, got %s", m.Architecture)
	}
	// One download for the shared config and one attempt at the missing
	// one; the index children and the artifact need none.
	if got := fake.Calls("GetBlob"); got != 2 {
		t.Errorf("GetBlob called %d times, want 2", got)
	}

	// A backend that cannot download blobs leaves platforms unknown.
	manifests = fetch(t, newRegistry(t, plain(fake), nil), "app", registry.FetchOptions{Platforms: true})
	if m := manifests[images[0]]; m.Architecture != "" {
		t.Errorf("platform = %s without a blob-capable backend", m.Architecture)
	}
}

func TestFetchPlatformErrors(t *testing.T) {
	fake := registrytest.New()
	image := registrytest.Image("app")
	config := fake.PutConfig("amd64", "linux")
	image.Config = &config
	fake.Add("app", image, registry.Attributes{Tags: []string{"v1"}})

	undecodable := registrytest.Image("undecodable")
	undecodable.Config = &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: godigest.Digest(fake.PutBlob([]byte("not json")))}
	fake.Add("undecodable", undecodable, registry.Attributes{Tags: []string{"v1"}})

	reg := newRegistry(t, fake, nil)
	platforms := registry.FetchOptions{Platforms: true}
	if _, _, err := reg.FetchRepositoryManifests(context.Background(), "undecodable", platforms); err == nil || !strings.Contains(err.Error(), "failed to decode image config") {
		t.Errorf("an undecodable config should fail the fetch, got %v", err)
	}

	// Only a missing config is tolerated; other failures are not.
	fake.Fail = func(op, _ string) error {
		if op == "GetBlob" {
			return errors.New("connection reset")
		}
		return nil
	}
	if _, _, err := reg.FetchRepositoryManifests(context.Background(), "app", platforms); err == nil || !strings.Contains(err.Error(), "failed to get image config") {
		t.Errorf("a failed config download should fail the fetch, got %v", err)
	}
}

func TestFetchMissing(t *testing.T) {
	fake := registrytest.New()
	present := fake.Add("app", registrytest.Image("present"), registry.Attributes{Tags: []string{"v1"}})
	gone := []registry.Attributes{
		{Digest: "sha256:" + strings.Repeat("1", 64), Tags: []string{"latest"}, ID: "7"},
		{Digest: "sha256:" + strings.Repeat("0", 64)},
	}
	for _, attrs := range gone {
		fake.AddMissing("app", attrs)
	}
	reg := newRegistry(t, fake, nil)
	ctx := context.Background()

	// A missing manifest is skipped but reported, with what the listing
	// said about it, sorted by digest.
	contents, found, err := reg.FetchRepositoryManifests(ctx, "app", registry.FetchOptions{IgnoreMissing: true})
	if err != nil || !found {
		t.Fatalf("FetchRepositoryManifests = %v, %v", found, err)
	}
	if len(contents.Manifests) != 1 || contents.Manifests[present] == nil {
		t.Errorf("a missing manifest should be skipped, got %d manifests", len(contents.Manifests))
	}
	if len(contents.Missing) != 2 || contents.Missing[0].Digest != gone[1].Digest ||
		contents.Missing[1].Digest != gone[0].Digest || contents.Missing[1].ID != "7" || !slices.Equal(contents.Missing[1].Tags, []string{"latest"}) {
		t.Errorf("missing = %+v, want both listed manifests with their attributes", contents.Missing)
	}
	if _, _, err := reg.FetchRepositoryManifests(ctx, "app", registry.FetchOptions{}); !registry.IsNotFound(err) {
		t.Errorf("a missing manifest should fail unless ignored, got %v", err)
	}

	contents, found, err = reg.FetchRepositoryManifests(ctx, "gone", registry.FetchOptions{IgnoreMissing: true})
	if err != nil || found || contents.Manifests != nil || contents.Missing != nil {
		t.Errorf("a missing repository should be reported as not found: %+v, %v, %v", contents, found, err)
	}
	if _, _, err := reg.FetchRepositoryManifests(ctx, "gone", registry.FetchOptions{}); !registry.IsNotFound(err) || !strings.Contains(err.Error(), "failed to list manifests for gone") {
		t.Errorf("a missing repository should fail unless ignored, got %v", err)
	}
}

// TestFetchRejectsBadContent: content is verified against the digest it was
// requested by, and the listing's digests must be well-formed — they become
// cache file names and URL path segments.
func TestFetchRejectsBadContent(t *testing.T) {
	fake := registrytest.New()
	fake.Put("tampered", []byte(`{"schemaVersion": 2}`), registry.Attributes{Digest: "sha256:" + strings.Repeat("a", 64)})
	fake.Put("invalid", []byte(`{"schemaVersion": 2}`), registry.Attributes{Digest: "sha256:../../escape"})
	fake.Put("unparsable", []byte(`not json`), registry.Attributes{})
	reg := newRegistry(t, fake, nil)

	for repository, want := range map[string]string{
		"tampered":   "registry returned content with digest",
		"invalid":    "invalid manifest digest",
		"unparsable": "failed to decode manifest",
	} {
		_, _, err := reg.FetchRepositoryManifests(context.Background(), repository, registry.FetchOptions{IgnoreMissing: true})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to mention %q", repository, err, want)
		}
	}
}

func TestFetchErrorsPropagate(t *testing.T) {
	fake := registrytest.New()
	fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	fake.Add("app", registrytest.Image("b"), registry.Attributes{})
	reg := newRegistry(t, fake, nil)

	boom := errors.New("boom")
	for _, op := range []string{"ListManifests", "GetManifest"} {
		fake.Fail = func(failing, _ string) error {
			if failing == op {
				return boom
			}
			return nil
		}
		if _, _, err := reg.FetchRepositoryManifests(context.Background(), "app", registry.FetchOptions{IgnoreMissing: true}); !errors.Is(err, boom) {
			t.Errorf("%s failure: error = %v, want it to wrap %v", op, err, boom)
		}
	}
}

func newCache(t *testing.T, dir string) *registry.Cache {
	t.Helper()
	cache, err := registry.NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestFetchUsesCache(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}})
	dir := t.TempDir()
	reg := newRegistry(t, fake, newCache(t, dir))

	fetch(t, reg, "app", registry.FetchOptions{})
	fetch(t, reg, "app", registry.FetchOptions{})
	if got := fake.Calls("GetManifest"); got != 1 {
		t.Errorf("GetManifest called %d times, want 1: the second fetch should read the cache", got)
	}

	// A corrupted cache entry fails verification and is downloaded afresh.
	if err := os.WriteFile(filepath.Join(dir, digest), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifests := fetch(t, reg, "app", registry.FetchOptions{})
	if got := fake.Calls("GetManifest"); got != 2 {
		t.Errorf("GetManifest called %d times, want 2 after corrupting the cache", got)
	}
	if manifests[digest] == nil || manifests[digest].Config == nil {
		t.Error("the re-downloaded manifest should be intact")
	}
}

// TestFetchWarnsOnceAboutCacheFailures: a cache that cannot be written to is
// reported, once, and the fetch goes on without it.
func TestFetchWarnsOnceAboutCacheFailures(t *testing.T) {
	fake := registrytest.New()
	for _, name := range []string{"a", "b", "c"} {
		fake.Add("app", registrytest.Image(name), registry.Attributes{})
	}
	dir := filepath.Join(t.TempDir(), "cache")
	reg, logs := newLoggedRegistry(t, fake, newCache(t, dir))
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if got := fetch(t, reg, "app", registry.FetchOptions{}); len(got) != 3 {
		t.Fatalf("fetched %d manifests, want 3", len(got))
	}
	if got := logs.Messages(slog.LevelWarn); len(got) != 1 || !strings.Contains(got[0], "cache") {
		t.Errorf("warnings = %q, want one about the cache", got)
	}
}

func TestListRepositories(t *testing.T) {
	fake := registrytest.New()
	fake.AddRepository("b")
	fake.AddRepository("a")
	reg := newRegistry(t, fake, nil)

	got, err := reg.ListRepositories(context.Background())
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Errorf("ListRepositories = %v, %v", got, err)
	}

	fake.Fail = func(string, string) error { return registrytest.Forbidden("catalog") }
	if _, err := reg.ListRepositories(context.Background()); !registry.IsPermissionError(err) {
		t.Errorf("listing error = %v, want the permission error", err)
	}
}

// catalogBackend lists the repositories it was given, whatever they are.
type catalogBackend struct {
	registry.Backend
	names []string
}

func (b catalogBackend) ListRepositories(context.Context) ([]string, error) {
	return b.names, nil
}

// TestListRepositoriesRejectsUnsafeNames: repository names from the catalog
// become cache paths and URL segments, so unsafe ones are refused.
func TestListRepositoriesRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{"../x", "app/../../etc", "UPPER", ""} {
		reg := newRegistry(t, catalogBackend{Backend: registrytest.New(), names: []string{"app", bad}}, nil)
		if got, err := reg.ListRepositories(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid repository name") {
			t.Errorf("catalog listing %q = %v, %v; want it refused", bad, got, err)
		}
	}
}

func TestDeleteManifests(t *testing.T) {
	fake := registrytest.New()
	keep := fake.Add("app", registrytest.Image("keep"), registry.Attributes{Tags: []string{"keep"}})
	drop := fake.Add("app", registrytest.Image("drop"), registry.Attributes{Tags: []string{"drop"}})
	cache := newCache(t, t.TempDir())
	reg := newRegistry(t, fake, cache)
	manifests := fetch(t, reg, "app", registry.FetchOptions{})

	if _, err := reg.DeleteManifests(context.Background(), []*registry.Manifest{manifests[drop]}, registry.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Digests("app"); !slices.Equal(got, []string{keep}) {
		t.Errorf("remaining = %v, want only %s", got, keep)
	}
	if cache.Get(drop) != nil {
		t.Error("a deleted manifest should be evicted from the cache")
	}
	if cache.Get(keep) == nil {
		t.Error("a kept manifest should stay cached")
	}

	fake.Fail = func(string, string) error { return registrytest.Forbidden("app") }
	_, err := reg.DeleteManifests(context.Background(), []*registry.Manifest{manifests[keep]}, registry.DeleteOptions{})
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "app@"+keep) {
		t.Errorf("delete error = %v, want the permission error naming the manifest", err)
	}
}

func TestDeleteRepository(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	cache := newCache(t, t.TempDir())
	reg := newRegistry(t, fake, cache)
	manifests := fetch(t, reg, "app", registry.FetchOptions{})

	if err := reg.DeleteRepository(context.Background(), "app", []*registry.Manifest{manifests[digest]}, registry.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("deleted repositories = %v", got)
	}
	if cache.Get(digest) != nil {
		t.Error("the manifests of a deleted repository should be evicted from the cache")
	}

	fake.Fail = func(string, string) error { return errors.New("boom") }
	if err := reg.DeleteRepository(context.Background(), "app", nil, registry.DeleteOptions{}); err == nil || !strings.Contains(err.Error(), "failed to delete repository app") {
		t.Errorf("delete error = %v", err)
	}
	if err := reg.DeleteRepository(context.Background(), "../app", nil, registry.DeleteOptions{}); err == nil || fake.Calls("DeleteRepository") != 2 {
		t.Errorf("an invalid repository name should be refused before any request, got %v", err)
	}
}

// lockedRepository stores, in fake's "app" repository, a locked manifest
// tagged v1 (locked) and latest, and an unlocked manifest tagged v2 (locked),
// and returns their fetched manifests with tag locks loaded.
func lockedRepository(t *testing.T, fake *registrytest.Backend, reg *registry.Registry) (locked, open *registry.Manifest) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	lockedDigest := fake.Add("app", registrytest.Image("locked"), registry.Attributes{Tags: []string{"v1", "latest"}, LastUpdated: old, Locked: true})
	openDigest := fake.Add("app", registrytest.Image("open"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
	fake.LockTag("app", "v1")
	fake.LockTag("app", "v2")
	fake.LockTag("app", "other") // on no manifest being deleted
	manifests := fetch(t, reg, "app", registry.FetchOptions{})
	locked, open = manifests[lockedDigest], manifests[openDigest]
	if err := reg.LoadTagLocks(context.Background(), "app", []*registry.Manifest{locked, open}); err != nil {
		t.Fatal(err)
	}
	return locked, open
}

func TestLoadTagLocks(t *testing.T) {
	fake := registrytest.New()
	reg := newRegistry(t, fake, nil)
	locked, open := lockedRepository(t, fake, reg)
	if !slices.Equal(locked.LockedTags, []string{"v1"}) || !slices.Equal(open.LockedTags, []string{"v2"}) {
		t.Errorf("LockedTags = %v and %v, want [v1] and [v2]", locked.LockedTags, open.LockedTags)
	}
	if fake.Calls("LockedTags") != 1 {
		t.Errorf("tags listed %d times, want once per repository", fake.Calls("LockedTags"))
	}

	// Loaded but not locked is empty rather than nil, which reads as not
	// loaded.
	unlocked := fake.Add("app", registrytest.Image("unlocked"), registry.Attributes{Tags: []string{"v3"}})
	m := fetch(t, reg, "app", registry.FetchOptions{})[unlocked]
	if err := reg.LoadTagLocks(context.Background(), "app", []*registry.Manifest{m}); err != nil || m.LockedTags == nil || len(m.LockedTags) != 0 {
		t.Errorf("LockedTags = %#v, %v; want loaded and empty", m.LockedTags, err)
	}

	fake.Fail = func(op, _ string) error {
		if op == "LockedTags" {
			return errors.New("boom")
		}
		return nil
	}
	if err := reg.LoadTagLocks(context.Background(), "app", []*registry.Manifest{m}); err == nil || !strings.Contains(err.Error(), "failed to list tags for app") {
		t.Errorf("a failure to list the tags should fail, got %v", err)
	}
}

// TestLoadTagLocksSkipsNeedlessListings: listing every tag of a repository is
// expensive, so it is skipped when no manifest is tagged, when the registry
// has no locks, and a repository gone meanwhile has no locks to load.
func TestLoadTagLocksSkipsNeedlessListings(t *testing.T) {
	fake := registrytest.New()
	untagged := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	tagged := fake.Add("app", registrytest.Image("b"), registry.Attributes{Tags: []string{"v1"}})
	fake.LockTag("app", "v1")
	manifests := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})
	ctx := context.Background()

	for name, load := range map[string]func() error{
		"untagged": func() error {
			return newRegistry(t, fake, nil).LoadTagLocks(ctx, "app", []*registry.Manifest{manifests[untagged]})
		},
		"empty": func() error { return newRegistry(t, fake, nil).LoadTagLocks(ctx, "app", nil) },
		"no locks": func() error {
			return newRegistry(t, plain(fake), nil).LoadTagLocks(ctx, "app", []*registry.Manifest{manifests[tagged]})
		},
	} {
		if err := load(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if fake.Calls("LockedTags") != 0 || manifests[tagged].LockedTags != nil {
		t.Errorf("tags listed %d times, LockedTags = %v; want no listing", fake.Calls("LockedTags"), manifests[tagged].LockedTags)
	}

	gone := &registry.Manifest{Repository: "gone", Attributes: registry.Attributes{Digest: tagged, Tags: []string{"v1"}}}
	if err := newRegistry(t, fake, nil).LoadTagLocks(ctx, "gone", []*registry.Manifest{gone}); err != nil || gone.LockedTags != nil {
		t.Errorf("a vanished repository = %v, %v; want nothing loaded and no error", gone.LockedTags, err)
	}
}

// TestDeleteManifestsUnlocks: with Unlock, every locked manifest and locked
// tag is unlocked right before its own deletion, and not otherwise.
func TestDeleteManifestsUnlocks(t *testing.T) {
	fake := registrytest.New()
	fake.EnforceLocks = true
	reg := newRegistry(t, fake, nil)
	locked, open := lockedRepository(t, fake, reg)
	toDelete := []*registry.Manifest{locked, open}

	_, err := reg.DeleteManifests(context.Background(), toDelete, registry.DeleteOptions{})
	if err == nil || len(fake.Deleted()) != 0 || len(fake.Unlocked()) != 0 {
		t.Fatalf("without Unlock: error %v, deleted %v, unlocked %v; want the locks to refuse the deletion", err, fake.Deleted(), fake.Unlocked())
	}

	if _, err := reg.DeleteManifests(context.Background(), toDelete, registry.DeleteOptions{Unlock: true}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"app:v1", "app:v2", "app@" + locked.Digest}; !slices.Equal(fake.Unlocked(), want) {
		t.Errorf("unlocked = %v, want %v", fake.Unlocked(), want)
	}
	if len(fake.Digests("app")) != 0 || len(fake.Relocked()) != 0 {
		t.Errorf("remaining %v, relocked %v; want everything deleted and nothing relocked", fake.Digests("app"), fake.Relocked())
	}
}

// TestDeleteManifestsWithoutLocks: registries without locks (GHCR) are asked
// for nothing but the deletions.
func TestDeleteManifestsWithoutLocks(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Locked: true})
	m := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})[digest]
	m.LockedTags = []string{"v1"}

	if _, err := newRegistry(t, plain(fake), nil).DeleteManifests(context.Background(), []*registry.Manifest{m}, registry.DeleteOptions{Unlock: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Unlocked()) != 0 || len(fake.Deleted()) != 1 {
		t.Errorf("unlocked %v, deleted %v; want only the deletion", fake.Unlocked(), fake.Deleted())
	}
}

// TestUnlockFailuresAreTolerated: an unlock that fails is logged, and the
// deletion is attempted anyway, reporting what could not be unlocked.
func TestUnlockFailuresAreTolerated(t *testing.T) {
	fake := registrytest.New()
	fake.EnforceLocks = true
	reg, logs := newLoggedRegistry(t, fake, nil)
	locked, open := lockedRepository(t, fake, reg)
	fake.Fail = func(op, _ string) error {
		if op == "UnlockManifest" || op == "UnlockTag" {
			return errors.New("boom")
		}
		return nil
	}

	_, err := reg.DeleteManifests(context.Background(), []*registry.Manifest{locked, open}, registry.DeleteOptions{Unlock: true})
	if got := fake.Calls("UnlockTag"); got != 2 {
		t.Errorf("UnlockTag called %d times, want once per locked tag", got)
	}
	if got := fake.Calls("UnlockManifest"); got != 1 {
		t.Errorf("UnlockManifest called %d times, want once", got)
	}
	if got := fake.Calls("DeleteManifest"); got != 2 {
		t.Errorf("DeleteManifest called %d times, want both deletions attempted", got)
	}
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(err.Error(), locked.Ref()) || !strings.Contains(err.Error(), open.Ref()) {
		t.Errorf("error = %v, want both lock refusals", err)
	}
	if got := logs.Messages(slog.LevelWarn); len(got) != 3 {
		t.Errorf("warnings = %q, want one per failed unlock", got)
	}
}

// TestFailedDeletionRelocks: a manifest whose deletion fails after unlocking
// is locked again, tags included, and so are its dependents, which are never
// unlocked in the first place.
func TestFailedDeletionRelocks(t *testing.T) {
	fake := registrytest.New()
	fake.EnforceLocks = true
	old := time.Now().Add(-48 * time.Hour)
	child := fake.Add("app", registrytest.Image("child"), registry.Attributes{LastUpdated: old, Locked: true})
	index := fake.Add("app", registrytest.Index(registrytest.Child(child, "linux", "amd64")), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, Locked: true})
	fake.LockTag("app", "v1")
	reg := newRegistry(t, fake, nil)
	manifests := fetch(t, reg, "app", registry.FetchOptions{})
	toDelete := []*registry.Manifest{manifests[index], manifests[child]}
	if err := reg.LoadTagLocks(context.Background(), "app", toDelete); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	fake.Fail = func(op, _ string) error {
		if op == "DeleteManifest" {
			return boom
		}
		return nil
	}

	_, err := reg.DeleteManifests(context.Background(), toDelete, registry.DeleteOptions{Unlock: true})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if want := []string{"app:v1", "app@" + index}; !slices.Equal(fake.Unlocked(), want) || !slices.Equal(fake.Relocked(), want) {
		t.Errorf("unlocked %v, relocked %v; want only the index and its tag, both relocked", fake.Unlocked(), fake.Relocked())
	}
	if got := fetch(t, reg, "app", registry.FetchOptions{}); !got[index].Locked || !got[child].Locked {
		t.Error("the manifests should be locked again")
	}
}

// interruptingBackend interrupts the run when asked to delete a manifest,
// keeping every optional interface of the fake. The deletion fails with err,
// or else with the interruption.
type interruptingBackend struct {
	*registrytest.Backend
	cancel context.CancelFunc
	err    error
}

func (b interruptingBackend) DeleteManifest(ctx context.Context, _ *registry.Manifest) error {
	b.cancel()
	if b.err != nil {
		return b.err
	}
	return ctx.Err()
}

// TestInterruptedDeletionRelocks: the run being interrupted is what fails a
// deletion then, and the locks are restored all the same.
func TestInterruptedDeletionRelocks(t *testing.T) {
	fake := registrytest.New()
	reg := newRegistry(t, fake, nil)
	locked, _ := lockedRepository(t, fake, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := newRegistry(t, interruptingBackend{Backend: fake, cancel: cancel}, nil).DeleteManifests(ctx, []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
	if want := []string{"app:v1", "app@" + locked.Digest}; !slices.Equal(fake.Relocked(), want) {
		t.Errorf("relocked = %v, want %v", fake.Relocked(), want)
	}
}

// TestInterruptedDeletionReportsRefusals: a deletion the registry refused
// just as the run was interrupted went unreported, as if the interruption
// had cut it short.
func TestInterruptedDeletionReportsRefusals(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}})
	m := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})[digest]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := interruptingBackend{Backend: fake, cancel: cancel, err: registrytest.Forbidden("app")}
	_, err := newRegistry(t, backend, nil).DeleteManifests(ctx, []*registry.Manifest{m}, registry.DeleteOptions{})
	if !errors.Is(err, context.Canceled) || !registry.IsPermissionError(err) {
		t.Errorf("error = %v, want both the interruption and the refusal", err)
	}
}

// lostUnlockBackend unlocks, but reports each unlock as failed, as when an
// interruption cuts off the response to a request the registry applied.
type lostUnlockBackend struct {
	*registrytest.Backend
}

var errLost = errors.New("response lost")

func (b lostUnlockBackend) UnlockManifest(ctx context.Context, m *registry.Manifest) (registry.Relock, error) {
	relock, _ := b.Backend.UnlockManifest(ctx, m)
	return relock, errLost
}

func (b lostUnlockBackend) UnlockTag(ctx context.Context, repository, tag string) (registry.Relock, error) {
	relock, _ := b.Backend.UnlockTag(ctx, repository, tag)
	return relock, errLost
}

// TestFailedUnlockRelocks: a failed unlock may have taken effect, and was
// never restored when the deletion then failed, manifest by manifest or of
// the whole repository.
func TestFailedUnlockRelocks(t *testing.T) {
	fake := registrytest.New()
	reg, logs := newLoggedRegistry(t, lostUnlockBackend{fake}, nil)
	locked, _ := lockedRepository(t, fake, reg)
	fake.Fail = func(op, _ string) error {
		if op == "DeleteManifest" || op == "DeleteRepository" || op == "RelockTag" {
			return errors.New("boom")
		}
		return nil
	}

	if _, err := reg.DeleteManifests(context.Background(), []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true}); err == nil {
		t.Fatal("the deletion should fail")
	}
	if got, want := fake.Relocked(), []string{"app@" + locked.Digest}; !slices.Equal(got, want) {
		t.Errorf("relocked %v, want %v", got, want)
	}
	if got := logs.Messages(slog.LevelWarn); !slices.ContainsFunc(got, func(msg string) bool {
		return strings.HasPrefix(msg, "Failed to restore lock after failed unlock and deletion; it may stay unlocked") && strings.Contains(msg, "app:v1")
	}) {
		t.Errorf("warnings %q, want one that the tag may stay unlocked", got)
	}

	if err := reg.DeleteRepository(context.Background(), "app", []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true}); err == nil {
		t.Fatal("the deletion should fail")
	}
	if got, want := fake.Relocked(), []string{"app@" + locked.Digest, "app@" + locked.Digest}; !slices.Equal(got, want) {
		t.Errorf("relocked %v after the repository deletion failed, want %v", got, want)
	}
}

// TestRelockFailuresAreLogged: a lock that cannot be restored is reported,
// naming what stays unlocked.
func TestRelockFailuresAreLogged(t *testing.T) {
	fake := registrytest.New()
	reg, logs := newLoggedRegistry(t, fake, nil)
	locked, _ := lockedRepository(t, fake, reg)
	fake.Fail = func(op, _ string) error {
		if op == "DeleteManifest" || op == "RelockTag" {
			return errors.New("boom")
		}
		return nil
	}

	if _, err := reg.DeleteManifests(context.Background(), []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true}); err == nil {
		t.Fatal("the deletion should fail")
	}
	if got := fake.Relocked(); !slices.Equal(got, []string{"app@" + locked.Digest}) {
		t.Errorf("relocked = %v, want the manifest", got)
	}
	if got := logs.Messages(slog.LevelWarn); len(got) != 1 || !strings.Contains(got[0], "app:v1") {
		t.Errorf("warnings = %q, want one naming the tag left unlocked", got)
	}
}

// TestDeleteRepositoryUnlocks: a whole-repository deletion unlocks what is
// locked right before it, and relocks it when it fails.
func TestDeleteRepositoryUnlocks(t *testing.T) {
	fake := registrytest.New()
	fake.EnforceLocks = true
	reg := newRegistry(t, fake, nil)
	locked, open := lockedRepository(t, fake, reg)
	known := []*registry.Manifest{locked, open}
	ctx := context.Background()

	if err := reg.DeleteRepository(ctx, "app", known, registry.DeleteOptions{}); err == nil || len(fake.DeletedRepositories()) != 0 {
		t.Fatalf("without Unlock: %v; want the locks to refuse the deletion", err)
	}

	boom := errors.New("boom")
	fake.Fail = func(op, _ string) error {
		if op == "DeleteRepository" {
			return boom
		}
		return nil
	}
	if err := reg.DeleteRepository(ctx, "app", known, registry.DeleteOptions{Unlock: true}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	want := []string{"app:v1", "app:v2", "app@" + locked.Digest}
	if !slices.Equal(fake.Unlocked(), want) || !slices.Equal(fake.Relocked(), want) {
		t.Errorf("unlocked %v, relocked %v; want %v, then relocked", fake.Unlocked(), fake.Relocked(), want)
	}

	fake.Fail = nil
	if err := reg.DeleteRepository(ctx, "app", known, registry.DeleteOptions{Unlock: true}); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("deleted repositories = %v", got)
	}
}

func TestProtectsLastTag(t *testing.T) {
	fake := registrytest.New()
	if newRegistry(t, fake, nil).ProtectsLastTag() {
		t.Error("the fake should not protect the last tag unless asked to")
	}
	fake.ProtectLastTag = true
	if !newRegistry(t, fake, nil).ProtectsLastTag() {
		t.Error("a protecting backend should be reported")
	}
	if newRegistry(t, plain(fake), nil).ProtectsLastTag() {
		t.Error("a backend without LastTagProtector does not protect the last tag")
	}
}

func newLoggedRegistry(t *testing.T, backend registry.Backend, cache *registry.Cache) (*registry.Registry, *registrytest.Recorder) {
	t.Helper()
	logs := &registrytest.Recorder{}
	reg, err := registry.New(backend, slog.New(logs), 4, cache)
	if err != nil {
		t.Fatal(err)
	}
	return reg, logs
}
