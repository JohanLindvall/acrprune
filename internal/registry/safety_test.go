package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
)

type interceptedBackend struct {
	registry.Backend
	list   func(context.Context, string, func(registry.Attributes) error) error
	get    func(context.Context, string, string) ([]byte, error)
	remove func(context.Context, *registry.Manifest) error
}

func (b interceptedBackend) ListManifests(ctx context.Context, repo string, fn func(registry.Attributes) error) error {
	if b.list != nil {
		return b.list(ctx, repo, fn)
	}
	return b.Backend.ListManifests(ctx, repo, fn)
}
func (b interceptedBackend) GetManifest(ctx context.Context, repo, digest string) ([]byte, error) {
	if b.get != nil {
		return b.get(ctx, repo, digest)
	}
	return b.Backend.GetManifest(ctx, repo, digest)
}
func (b interceptedBackend) DeleteManifest(ctx context.Context, m *registry.Manifest) error {
	if b.remove != nil {
		return b.remove(ctx, m)
	}
	return b.Backend.DeleteManifest(ctx, m)
}

func TestDeleteParentsBeforeChildrenAndStopOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		fake := registrytest.New()
		idx, amd, arm := multiArch(fake, time.Now())
		backend := interceptedBackend{Backend: fake, remove: func(ctx context.Context, m *registry.Manifest) error {
			if m.Digest == idx && fail {
				return registrytest.Forbidden("index")
			}
			if m.Digest != idx {
				for _, digest := range fake.Digests("app") {
					if digest == idx {
						t.Error("child deleted before index")
					}
				}
			}
			return fake.DeleteManifest(ctx, m)
		}}
		reg := newRegistry(t, backend, nil)
		all := fetch(t, reg, "app", registry.FetchOptions{})
		_, err := reg.DeleteManifests(t.Context(), []*registry.Manifest{all[amd], all[arm], all[idx]}, registry.DeleteOptions{})
		if (err != nil) != fail {
			t.Fatalf("fail=%v: error=%v", fail, err)
		}
		if fail && len(fake.Deleted()) != 0 {
			t.Fatalf("children deleted despite locked index: %v", fake.Deleted())
		}
		if !fail && len(fake.Digests("app")) != 0 {
			t.Fatal("deletion incomplete")
		}
	}
}

// signed stores in fake's "app" repository an image tagged v1 with an OCI 1.1
// signature (subject) and a cosign signature (tag scheme), and returns their
// digests.
func signed(fake *registrytest.Backend) (image, signature, cosign string) {
	old := time.Now().Add(-48 * time.Hour)
	image = fake.Add("app", registrytest.Image("image"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	sig := registrytest.Image("signature")
	sig.Subject = &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: godigest.Digest(image)}
	signature = fake.Add("app", sig, registry.Attributes{LastUpdated: old})
	cosign = fake.Add("app", registrytest.Image("cosign"), registry.Attributes{Tags: []string{tagOf(image) + ".sig"}, LastUpdated: old})
	return image, signature, cosign
}

// tagOf returns the cosign tag stem naming a digest: sha256-<hex>.
func tagOf(digest string) string {
	return strings.Replace(digest, ":", "-", 1)
}

// TestDeleteSubjectBeforeReferrers: an image that fails to delete — locked,
// say — keeps its signatures, whether they name it by subject or by tag.
func TestDeleteSubjectBeforeReferrers(t *testing.T) {
	for _, fail := range []bool{false, true} {
		fake := registrytest.New()
		image, signature, cosign := signed(fake)
		backend := interceptedBackend{Backend: fake, remove: func(ctx context.Context, m *registry.Manifest) error {
			if m.Digest == image && fail {
				return registrytest.Forbidden("image")
			}
			if m.Digest != image && slices.Contains(fake.Digests("app"), image) {
				t.Errorf("fail=%v: referrer %s deleted before its subject", fail, m.Digest)
			}
			return fake.DeleteManifest(ctx, m)
		}}
		reg := newRegistry(t, backend, nil)
		all := fetch(t, reg, "app", registry.FetchOptions{})
		_, err := reg.DeleteManifests(t.Context(), []*registry.Manifest{all[signature], all[cosign], all[image]}, registry.DeleteOptions{})
		if (err != nil) != fail {
			t.Fatalf("fail=%v: error=%v", fail, err)
		}
		if fail && !slices.Equal(fake.Digests("app"), slices.Sorted(slices.Values([]string{image, signature, cosign}))) {
			t.Errorf("remaining = %v; the kept image should keep its signatures", fake.Digests("app"))
		}
		if !fail && len(fake.Digests("app")) != 0 {
			t.Errorf("remaining = %v, want everything deleted", fake.Digests("app"))
		}
	}
}

// TestDeleteIndexReferencingItsSubject: an index whose subject is also one of
// its children is deleted first, as an index, rather than forming a cycle.
func TestDeleteIndexReferencingItsSubject(t *testing.T) {
	fake := registrytest.New()
	child := fake.Add("app", registrytest.Image("child"), registry.Attributes{})
	index := registrytest.Index(registrytest.Child(child, "linux", "amd64"))
	index.Subject = &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: godigest.Digest(child)}
	parent := fake.Add("app", index, registry.Attributes{Tags: []string{"v1"}})
	var order []string
	backend := interceptedBackend{Backend: fake, remove: func(ctx context.Context, m *registry.Manifest) error {
		order = append(order, m.Digest)
		return fake.DeleteManifest(ctx, m)
	}}
	reg := newRegistry(t, backend, nil)
	all := fetch(t, reg, "app", registry.FetchOptions{})
	if _, err := reg.DeleteManifests(t.Context(), []*registry.Manifest{all[child], all[parent]}, registry.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{parent, child}) {
		t.Errorf("deletion order = %v, want the index %s first", order, parent)
	}
}

// TestDeleteContinuesPastFailures: a failed deletion neither cancels the
// requests in flight nor stops independent deletions; it only keeps what
// depends on it, transitively. Every failure is reported.
func TestDeleteContinuesPastFailures(t *testing.T) {
	fake := registrytest.New()
	failing := fake.Add("app", registrytest.Image("failing"), registry.Attributes{Tags: []string{"locked"}})
	leaf := fake.Add("app", registrytest.Image("leaf"), registry.Attributes{})
	inner := fake.Add("app", registrytest.Index(registrytest.Child(leaf, "linux", "amd64")), registry.Attributes{})
	outer := fake.Add("app", registrytest.Index(registrytest.Child(inner, "linux", "amd64")), registry.Attributes{Tags: []string{"v1"}})
	var independent []string
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		independent = append(independent, fake.Add("app", registrytest.Image(name), registry.Attributes{}))
	}
	boom := errors.New("boom")
	failed := make(chan struct{})
	var mu sync.Mutex
	canceled := false
	backend := interceptedBackend{Backend: fake, remove: func(ctx context.Context, m *registry.Manifest) error {
		switch m.Digest {
		case failing:
			defer close(failed)
			return registrytest.Forbidden("locked manifest")
		case outer:
			return boom
		case independent[0]:
			// In flight while the other deletion fails.
			<-failed
			select {
			case <-ctx.Done():
				mu.Lock()
				canceled = true
				mu.Unlock()
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return fake.DeleteManifest(ctx, m)
	}}
	reg, logs := newLoggedRegistry(t, backend, nil)
	all := fetch(t, reg, "app", registry.FetchOptions{})

	_, err := reg.DeleteManifests(t.Context(), slices.Collect(maps.Values(all)), registry.DeleteOptions{})
	if !registry.IsPermissionError(err) || !errors.Is(err, boom) {
		t.Errorf("error = %v, want both failures", err)
	}
	if canceled {
		t.Error("a failed deletion canceled another in flight")
	}
	var want []string
	for _, digest := range independent {
		want = append(want, "app@"+digest)
	}
	if got := fake.Deleted(); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("deleted = %v, want the independent manifests %v", got, want)
	}
	if got := logs.Messages(slog.LevelWarn); len(got) != 2 {
		t.Errorf("warnings = %q, want one per manifest kept for its failed parent", got)
	}
}

// TestDeletionBatchesValidateFirst: a selection that cannot be ordered safely
// is refused before anything is deleted.
func TestDeletionBatchesValidateFirst(t *testing.T) {
	fake := registrytest.New()
	a := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	b := fake.Add("app", registrytest.Image("b"), registry.Attributes{})
	reg := newRegistry(t, fake, nil)
	all := fetch(t, reg, "app", registry.FetchOptions{})
	cyclicA, cyclicB := *all[a], *all[b]
	cyclicA.Manifests = []v1.Descriptor{{Digest: godigest.Digest(b)}}
	cyclicB.Manifests = []v1.Descriptor{{Digest: godigest.Digest(a)}}
	badDigest := *all[a]
	badDigest.Digest = "sha256:short"
	badRepository := *all[a]
	badRepository.Repository = "../app"

	for name, selection := range map[string][]*registry.Manifest{
		"nil manifest":       {all[a], nil},
		"invalid digest":     {all[b], &badDigest},
		"invalid repository": {&badRepository},
		"duplicate":          {all[a], all[b], all[a]},
		"cycle":              {&cyclicA, &cyclicB},
	} {
		if _, err := reg.DeleteManifests(t.Context(), selection, registry.DeleteOptions{}); err == nil {
			t.Errorf("%s: deletion should be refused", name)
		}
	}
	if got := fake.Calls("DeleteManifest"); got != 0 {
		t.Errorf("%d deletions attempted, want none", got)
	}
}

func TestDuplicateListingFailsClosed(t *testing.T) {
	fake := registrytest.New()
	fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	b := interceptedBackend{Backend: fake, list: func(ctx context.Context, repo string, fn func(registry.Attributes) error) error {
		return fake.ListManifests(ctx, repo, func(a registry.Attributes) error {
			if err := fn(a); err != nil {
				return err
			}
			return fn(a)
		})
	}}
	_, _, err := newRegistry(t, b, nil).FetchRepositoryManifests(t.Context(), "app", registry.FetchOptions{})
	if !errors.Is(err, registry.ErrRepositoryChanged) || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("error=%v", err)
	}
}

func TestListingFailureCancelsDownloads(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	boom := errors.New("listing failed")
	b := interceptedBackend{Backend: fake,
		list: func(ctx context.Context, repo string, fn func(registry.Attributes) error) error {
			if err := fn(registry.Attributes{Digest: digest}); err != nil {
				return err
			}
			return boom
		},
		get: func(ctx context.Context, _, _ string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() },
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := newRegistry(t, b, nil).FetchRepositoryManifests(ctx, "app", registry.FetchOptions{})
	if !errors.Is(err, boom) || ctx.Err() != nil {
		t.Fatalf("listing failure did not promptly cancel downloads: %v, %v", err, ctx.Err())
	}
}

// TestDownloadFailureStopsListing: a download failing while the listing is
// still going stops it, and the download's error, not the listing's
// cancellation, is reported.
func TestDownloadFailureStopsListing(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	boom := errors.New("download failed")
	b := interceptedBackend{Backend: fake,
		list: func(ctx context.Context, repo string, fn func(registry.Attributes) error) error {
			if err := fn(registry.Attributes{Digest: digest}); err != nil {
				return err
			}
			<-ctx.Done() // more pages to come
			return ctx.Err()
		},
		get: func(context.Context, string, string) ([]byte, error) { return nil, boom },
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := newRegistry(t, b, nil).FetchRepositoryManifests(ctx, "app", registry.FetchOptions{})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "failed to get manifest app@"+digest) || ctx.Err() != nil {
		t.Fatalf("error = %v (deadline: %v), want the download failure", err, ctx.Err())
	}
}

// TestFetchRejectsOversizedDocuments: a runaway response is refused rather
// than read into memory and cached.
func TestFetchRejectsOversizedDocuments(t *testing.T) {
	fake := registrytest.New()
	fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	b := interceptedBackend{Backend: fake, get: func(context.Context, string, string) ([]byte, error) {
		return make([]byte, registry.MaxDocumentSize+1), nil
	}}
	_, _, err := newRegistry(t, b, nil).FetchRepositoryManifests(t.Context(), "app", registry.FetchOptions{})
	if err == nil || !strings.Contains(err.Error(), "document exceeds") {
		t.Errorf("error = %v, want the oversized document refused", err)
	}
}

// TestVerifyRepositorySnapshot: every change between inspection and a
// whole-repository deletion is caught.
func TestVerifyRepositorySnapshot(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	tests := []struct {
		name string
		// change alters the repository after inspection; loaded[0] is
		// the tagged manifest, loaded[1] the untagged one.
		change    func(t *testing.T, fake *registrytest.Backend, loaded []*registry.Manifest)
		tagLocks  bool // load tag locks before the change
		duplicate bool // list a manifest twice
		cancel    bool
		want      error
	}{
		{name: "unchanged"},
		{name: "unchanged with tag locks", tagLocks: true},
		{name: "manifest removed", want: registry.ErrRepositoryChanged,
			change: func(t *testing.T, fake *registrytest.Backend, loaded []*registry.Manifest) {
				if err := fake.DeleteManifest(t.Context(), loaded[1]); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "updated", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("tagged"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: time.Now(), ID: "1"})
			}},
		{name: "replaced", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("tagged"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, ID: "2"})
			}},
		{name: "tag added", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("untagged"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
			}},
		{name: "tag removed", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("tagged"), registry.Attributes{LastUpdated: old, ID: "1"})
			}},
		{name: "locked", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("untagged"), registry.Attributes{LastUpdated: old, Locked: true})
			}},
		{name: "tag locked", tagLocks: true, want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.LockTag("app", "v1")
			}},
		// Without tag locks loaded there is nothing to compare them with.
		{name: "tag locked, tag locks not loaded",
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.LockTag("app", "v1")
			}},
		{name: "new manifest", want: registry.ErrRepositoryChanged,
			change: func(_ *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				fake.Add("app", registrytest.Image("new"), registry.Attributes{LastUpdated: time.Now()})
			}},
		{name: "listed twice", duplicate: true, want: registry.ErrRepositoryChanged},
		{name: "repository deleted", want: registry.ErrRepositoryGone,
			change: func(t *testing.T, fake *registrytest.Backend, _ []*registry.Manifest) {
				if err := fake.DeleteRepository(t.Context(), "app"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "canceled", cancel: true, want: context.Canceled},
	}
	for _, tt := range tests {
		fake := registrytest.New()
		tagged := fake.Add("app", registrytest.Image("tagged"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, ID: "1"})
		untagged := fake.Add("app", registrytest.Image("untagged"), registry.Attributes{LastUpdated: old})
		var backend registry.Backend = fake
		if tt.duplicate {
			backend = interceptedBackend{Backend: fake, list: func(ctx context.Context, repo string, fn func(registry.Attributes) error) error {
				return fake.ListManifests(ctx, repo, func(a registry.Attributes) error {
					if err := fn(a); err != nil {
						return err
					}
					return fn(a)
				})
			}}
		}
		reg := newRegistry(t, backend, nil)
		all := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})
		loaded := []*registry.Manifest{all[tagged], all[untagged]}
		if tt.tagLocks {
			if err := reg.LoadTagLocks(t.Context(), "app", loaded); err != nil {
				t.Fatal(err)
			}
		}
		if tt.change != nil {
			tt.change(t, fake, loaded)
		}
		ctx, cancel := context.WithCancel(t.Context())
		if tt.cancel {
			cancel()
		}
		err := reg.VerifyRepositorySnapshot(ctx, "app", loaded)
		cancel()
		if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
		}
		if errors.Is(tt.want, registry.ErrRepositoryChanged) && !strings.Contains(err.Error(), "refusing to delete from repository app") {
			t.Errorf("%s: error = %v, want it to say the deletion is refused", tt.name, err)
		}
		if errors.Is(tt.want, registry.ErrRepositoryGone) && !registry.IsNotFound(err) {
			t.Errorf("%s: error = %v, want the not-found response kept in the chain", tt.name, err)
		}
	}
}

// TestVerifyRepositorySnapshotListingFailure: a failed listing is reported as
// such, not as a change.
func TestVerifyRepositorySnapshotListingFailure(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	reg := newRegistry(t, fake, nil)
	all := fetch(t, reg, "app", registry.FetchOptions{})
	fake.Fail = func(op, _ string) error { return registrytest.Forbidden("app") }
	err := reg.VerifyRepositorySnapshot(t.Context(), "app", []*registry.Manifest{all[digest]})
	if !registry.IsPermissionError(err) || errors.Is(err, registry.ErrRepositoryChanged) {
		t.Errorf("error = %v, want the permission error", err)
	}
}

// TestFetchDerivesTagSubjects: manifests that name their subject by the cosign
// or OCI referrers tag scheme are recognized as referrers — but only when the
// subject is in the same repository, and without touching their documents.
func TestFetchDerivesTagSubjects(t *testing.T) {
	fake := registrytest.New()
	image, _, cosign := signed(fake)
	att := fake.Add("app", registrytest.Image("attestation"), registry.Attributes{Tags: []string{tagOf(image) + ".att", tagOf(image) + ".sbom"}})
	fallback := fake.Add("app", registrytest.Index(registrytest.Child(att, "unknown", "unknown")), registry.Attributes{Tags: []string{tagOf(image)}})
	elsewhere := fake.Add("app", registrytest.Image("elsewhere"), registry.Attributes{Tags: []string{tagOf(godigest.FromString("not here").String()) + ".sig"}})
	mixed := fake.Add("app", registrytest.Image("mixed"), registry.Attributes{Tags: []string{tagOf(image) + ".sig", "latest"}})
	declared := registrytest.Image("declared")
	declared.Subject = &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: godigest.Digest(att)}
	oci := fake.Add("app", declared, registry.Attributes{Tags: []string{tagOf(image) + ".sig"}})
	raw, err := json.Marshal(registrytest.Image("cosign"))
	if err != nil {
		t.Fatal(err)
	}
	cache := newCache(t, t.TempDir())

	all := fetch(t, newRegistry(t, fake, cache), "app", registry.FetchOptions{})
	for digest, want := range map[string]string{
		image:     "",
		cosign:    image,
		att:       image,
		fallback:  image,
		elsewhere: "", // COSIGN_REPOSITORY, or dangling
		mixed:     "",
		oci:       att, // the OCI subject wins
	} {
		m := all[digest]
		if m.SubjectDigest() != want {
			t.Errorf("%v: SubjectDigest = %q, want %q", m.Tags, m.SubjectDigest(), want)
		}
		if m.Subject == nil && m.TagSubject != want {
			t.Errorf("%v: TagSubject = %q, want %q", m.Tags, m.TagSubject, want)
		}
	}
	if all[cosign].Subject != nil || string(cache.Get(cosign)) != string(raw) {
		t.Error("the document of a tag-scheme referrer should be left as it is")
	}
}

// TestFetchReportsUnsupportedManifests: a legacy manifest is reported as such,
// even when its digest covers only part of the document as a signed Docker
// schema 1 manifest's does, rather than as tampered content.
func TestFetchReportsUnsupportedManifests(t *testing.T) {
	fake := registrytest.New()
	schema1 := []byte(`{"schemaVersion": 1, "name": "app", "tag": "old", "signatures": [{"protected": "..."}]}`)
	payload := godigest.FromString(`{"schemaVersion": 1, "name": "app", "tag": "old"}`).String()
	fake.Put("app", schema1, registry.Attributes{Digest: payload, Tags: []string{"old"}})
	fake.Add("app", registrytest.Image("current"), registry.Attributes{Tags: []string{"v1"}})
	artifact := registrytest.Image("artifact")
	artifact.MediaType = "application/vnd.oci.artifact.manifest.v1+json"
	fake.Add("artifact", artifact, registry.Attributes{})
	reg := newRegistry(t, fake, newCache(t, t.TempDir()))

	for repository, want := range map[string]string{"app": "schema version 1", "artifact": "application/vnd.oci.artifact.manifest.v1+json"} {
		_, _, err := reg.FetchRepositoryManifests(t.Context(), repository, registry.FetchOptions{IgnoreMissing: true})
		if !errors.Is(err, registry.ErrUnsupportedManifest) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), repository+"@") ||
			strings.Contains(err.Error(), "registry returned content") || strings.Count(err.Error(), repository+"@") != 1 {
			t.Errorf("%s: error = %v, want an unsupported manifest naming it once and %q", repository, err, want)
		}
	}
}
