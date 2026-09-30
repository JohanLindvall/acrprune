package registry_test

import (
	"context"
	"errors"
	"log/slog"
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

func TestFetchUsesCache(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}})
	dir := t.TempDir()
	reg := newRegistry(t, fake, registry.NewCache(dir))

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

func TestDeleteManifests(t *testing.T) {
	fake := registrytest.New()
	keep := fake.Add("app", registrytest.Image("keep"), registry.Attributes{Tags: []string{"keep"}})
	drop := fake.Add("app", registrytest.Image("drop"), registry.Attributes{Tags: []string{"drop"}})
	cache := registry.NewCache(t.TempDir())
	reg := newRegistry(t, fake, cache)
	manifests := fetch(t, reg, "app", registry.FetchOptions{})

	if err := reg.DeleteManifests(context.Background(), []*registry.Manifest{manifests[drop]}); err != nil {
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
	err := reg.DeleteManifests(context.Background(), []*registry.Manifest{manifests[keep]})
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "app@"+keep) {
		t.Errorf("delete error = %v, want the permission error naming the manifest", err)
	}
}

func TestDeleteRepository(t *testing.T) {
	fake := registrytest.New()
	fake.AddRepository("app")
	reg := newRegistry(t, fake, nil)

	if err := reg.DeleteRepository(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("deleted repositories = %v", got)
	}

	fake.Fail = func(string, string) error { return errors.New("boom") }
	if err := reg.DeleteRepository(context.Background(), "app"); err == nil || !strings.Contains(err.Error(), "failed to delete repository app") {
		t.Errorf("delete error = %v", err)
	}
}

func TestUnlockManifests(t *testing.T) {
	fake := registrytest.New()
	locked := fake.Add("app", registrytest.Image("locked"), registry.Attributes{Tags: []string{"v1", "latest"}, Locked: true})
	open := fake.Add("app", registrytest.Image("open"), registry.Attributes{Tags: []string{"v2"}})
	fake.LockTag("app", "latest")
	fake.LockTag("app", "v2")
	fake.LockTag("app", "other") // on no manifest being deleted
	reg := newRegistry(t, fake, nil)
	manifests := fetch(t, reg, "app", registry.FetchOptions{})
	toDelete := []*registry.Manifest{manifests[locked], manifests[open]}

	if err := reg.UnlockManifests(context.Background(), "app", toDelete); err != nil {
		t.Fatal(err)
	}
	want := []string{"app:latest", "app:v2", "app@" + locked}
	if got := fake.Unlocked(); !slices.Equal(got, want) {
		t.Errorf("unlocked = %v, want %v", got, want)
	}

	// Unlock failures are tolerated: the delete that follows reports them.
	fake.Fail = func(op, _ string) error {
		if op == "UnlockManifest" || op == "UnlockTag" {
			return errors.New("boom")
		}
		return nil
	}
	if err := reg.UnlockManifests(context.Background(), "app", toDelete); err != nil {
		t.Errorf("unlock failures should be tolerated, got %v", err)
	}

	// Failing to find the locked tags is not.
	fake.Fail = func(op, _ string) error {
		if op == "LockedTags" {
			return errors.New("boom")
		}
		return nil
	}
	if err := reg.UnlockManifests(context.Background(), "app", toDelete); err == nil {
		t.Error("a failure to list locked tags should fail the unlock")
	}
}

// TestUnlockManifestsWithoutLocks: registries without locks (GHCR) and empty
// deletions make no requests.
func TestUnlockManifestsWithoutLocks(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Locked: true})
	manifests := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})

	if err := newRegistry(t, plain(fake), nil).UnlockManifests(context.Background(), "app", []*registry.Manifest{manifests[digest]}); err != nil {
		t.Fatal(err)
	}
	if err := newRegistry(t, fake, nil).UnlockManifests(context.Background(), "app", nil); err != nil {
		t.Fatal(err)
	}
	if fake.Calls("LockedTags") != 0 || len(fake.Unlocked()) != 0 {
		t.Errorf("nothing should be unlocked: %v", fake.Unlocked())
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
