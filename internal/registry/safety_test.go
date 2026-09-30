package registry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		err := reg.DeleteManifests(t.Context(), []*registry.Manifest{all[amd], all[arm], all[idx]})
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
	if err == nil || !strings.Contains(err.Error(), "more than once") {
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

func TestRecheckDetectsNewPushAndRetag(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	reg := newRegistry(t, fake, nil)
	all := fetch(t, reg, "app", registry.FetchOptions{})
	if err := reg.VerifyRepositorySnapshot(t.Context(), "app", []*registry.Manifest{all[digest]}); err != nil {
		t.Fatal(err)
	}
	fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
	if err := reg.VerifyRepositorySnapshot(t.Context(), "app", []*registry.Manifest{all[digest]}); err == nil {
		t.Fatal("retag not detected")
	}
	fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, Locked: true})
	if err := reg.VerifyRepositorySnapshot(t.Context(), "app", []*registry.Manifest{all[digest]}); err == nil {
		t.Fatal("new lock not detected")
	}
	fake.Add("app", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("app", registrytest.Image("new"), registry.Attributes{LastUpdated: time.Now()})
	if err := reg.VerifyRepositorySnapshot(t.Context(), "app", []*registry.Manifest{all[digest]}); err == nil {
		t.Fatal("new push not detected")
	}
}
