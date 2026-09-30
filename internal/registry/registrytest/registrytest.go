// Package registrytest provides an in-memory registry.Backend for tests.
package registrytest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	godigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

var (
	_ registry.Backend          = (*Backend)(nil)
	_ registry.Unlocker         = (*Backend)(nil)
	_ registry.BlobGetter       = (*Backend)(nil)
	_ registry.LastTagProtector = (*Backend)(nil)
)

// Backend is an in-memory registry. It implements registry.Backend and every
// optional backend interface; wrap it as struct{ registry.Backend }{b} to
// present a backend without the optional ones.
type Backend struct {
	// ProtectLastTag makes the backend refuse, as GHCR does, to delete the
	// last tagged manifest of a repository, and report so through
	// ProtectsLastTag.
	ProtectLastTag bool
	// Fail, when set, is called before every operation with the operation's
	// method name and repository ("" for ListRepositories), and may return
	// an error for the operation to fail with.
	Fail func(op, repository string) error

	mu           sync.Mutex
	repositories map[string]*repository
	blobs        map[string][]byte
	lockedTags   map[string]map[string]bool
	calls        map[string]int
	deleted      []string
	deletedRepos []string
	unlocked     []string
}

type repository struct {
	order     []string // digests in listing order
	manifests map[string]*entry
}

type entry struct {
	attrs registry.Attributes
	raw   []byte // nil when listed but missing
}

// New returns an empty registry.
func New() *Backend {
	return &Backend{
		repositories: map[string]*repository{},
		blobs:        map[string][]byte{},
		lockedTags:   map[string]map[string]bool{},
		calls:        map[string]int{},
	}
}

// NotFound returns the error the backend fails with on a missing resource.
func NotFound(what string) error {
	return &registry.ResponseError{StatusCode: http.StatusNotFound, Err: fmt.Errorf("%s not found", what)}
}

// Forbidden returns a permission error, for injecting through Fail.
func Forbidden(what string) error {
	return &registry.ResponseError{StatusCode: http.StatusForbidden, Err: fmt.Errorf("access to %s denied", what)}
}

// AddRepository creates an empty repository.
func (b *Backend) AddRepository(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.repo(name)
}

// repo returns the named repository, creating it. b.mu must be held.
func (b *Backend) repo(name string) *repository {
	r := b.repositories[name]
	if r == nil {
		r = &repository{manifests: map[string]*entry{}}
		b.repositories[name] = r
	}
	return r
}

// Put stores a manifest document in the repository, listed with attrs, and
// returns its digest. The digest is computed from raw unless attrs names one,
// which makes the stored content fail verification.
func (b *Backend) Put(repository string, raw []byte, attrs registry.Attributes) string {
	if attrs.Digest == "" {
		attrs.Digest = godigest.FromBytes(raw).String()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.repo(repository)
	if _, ok := r.manifests[attrs.Digest]; !ok {
		r.order = append(r.order, attrs.Digest)
	}
	r.manifests[attrs.Digest] = &entry{attrs: attrs, raw: raw}
	return attrs.Digest
}

// Add stores a manifest document built from doc and returns its digest.
func (b *Backend) Add(repository string, doc registry.OCIManifest, attrs registry.Attributes) string {
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b.Put(repository, raw, attrs)
}

// AddMissing lists a manifest whose download fails as not found.
func (b *Backend) AddMissing(repository string, attrs registry.Attributes) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.repo(repository)
	r.order = append(r.order, attrs.Digest)
	r.manifests[attrs.Digest] = &entry{attrs: attrs}
}

// PutBlob stores a blob and returns its digest.
func (b *Backend) PutBlob(content []byte) string {
	digest := godigest.FromBytes(content).String()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blobs[digest] = content
	return digest
}

// PutConfig stores an image config for the platform and returns its
// descriptor.
func (b *Backend) PutConfig(architecture, os string) v1.Descriptor {
	content, err := json.Marshal(map[string]string{"architecture": architecture, "os": os})
	if err != nil {
		panic(err)
	}
	digest := b.PutBlob(content)
	return v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: godigest.Digest(digest), Size: int64(len(content))}
}

// LockTag locks a tag against deletion.
func (b *Backend) LockTag(repository, tag string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lockedTags[repository] == nil {
		b.lockedTags[repository] = map[string]bool{}
	}
	b.lockedTags[repository][tag] = true
}

// Calls returns how often the operation with the given method name ran.
func (b *Backend) Calls(op string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[op]
}

// Deleted returns the references of the deleted manifests, sorted.
func (b *Backend) Deleted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(slices.Values(b.deleted))
}

// DeletedRepositories returns the deleted repositories, sorted.
func (b *Backend) DeletedRepositories() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(slices.Values(b.deletedRepos))
}

// Unlocked returns what was unlocked, sorted: manifest references, and tags
// as repository:tag.
func (b *Backend) Unlocked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(slices.Values(b.unlocked))
}

// Digests returns the digests remaining in the repository, sorted.
func (b *Backend) Digests(repository string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.repositories[repository]
	if r == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(r.manifests))
}

// begin counts an operation and consults Fail.
func (b *Backend) begin(op, repository string) error {
	b.mu.Lock()
	b.calls[op]++
	b.mu.Unlock()
	if b.Fail != nil {
		return b.Fail(op, repository)
	}
	return nil
}

func (b *Backend) ListRepositories(context.Context) ([]string, error) {
	if err := b.begin("ListRepositories", ""); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(maps.Keys(b.repositories)), nil
}

func (b *Backend) ListManifests(_ context.Context, repository string, fn func(registry.Attributes) error) error {
	if err := b.begin("ListManifests", repository); err != nil {
		return err
	}
	b.mu.Lock()
	r := b.repositories[repository]
	var listing []registry.Attributes
	if r != nil {
		for _, digest := range r.order {
			if e := r.manifests[digest]; e != nil {
				attrs := e.attrs
				attrs.Tags = slices.Clone(attrs.Tags)
				listing = append(listing, attrs)
			}
		}
	}
	b.mu.Unlock()
	if r == nil {
		return NotFound("repository " + repository)
	}
	for _, attrs := range listing {
		if err := fn(attrs); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) GetManifest(_ context.Context, repository, digest string) ([]byte, error) {
	if err := b.begin("GetManifest", repository); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.repositories[repository]; r != nil {
		if e := r.manifests[digest]; e != nil && e.raw != nil {
			return slices.Clone(e.raw), nil
		}
	}
	return nil, NotFound("manifest " + repository + "@" + digest)
}

func (b *Backend) GetBlob(_ context.Context, repository, digest string) ([]byte, error) {
	if err := b.begin("GetBlob", repository); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if content, ok := b.blobs[digest]; ok {
		return slices.Clone(content), nil
	}
	return nil, NotFound("blob " + digest)
}

func (b *Backend) DeleteManifest(_ context.Context, m *registry.Manifest) error {
	if err := b.begin("DeleteManifest", m.Repository); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.repositories[m.Repository]
	if r == nil || r.manifests[m.Digest] == nil {
		return nil // already gone
	}
	if b.ProtectLastTag && len(r.manifests[m.Digest].attrs.Tags) > 0 && tagged(r) == 1 {
		return &registry.ResponseError{StatusCode: http.StatusBadRequest,
			Err: fmt.Errorf("deleting %s: you cannot delete the last tagged version of a package; you must delete the package instead", m.Ref())}
	}
	delete(r.manifests, m.Digest)
	b.deleted = append(b.deleted, m.Ref())
	return nil
}

// tagged counts the tagged manifests of a repository.
func tagged(r *repository) int {
	n := 0
	for _, e := range r.manifests {
		if len(e.attrs.Tags) > 0 {
			n++
		}
	}
	return n
}

func (b *Backend) DeleteRepository(_ context.Context, repository string) error {
	if err := b.begin("DeleteRepository", repository); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.repositories, repository)
	b.deletedRepos = append(b.deletedRepos, repository)
	return nil
}

func (b *Backend) LockedTags(_ context.Context, repository string) (map[string]bool, error) {
	if err := b.begin("LockedTags", repository); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.lockedTags[repository]), nil
}

func (b *Backend) UnlockManifest(_ context.Context, m *registry.Manifest) error {
	if err := b.begin("UnlockManifest", m.Repository); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.repositories[m.Repository]; r != nil {
		if e := r.manifests[m.Digest]; e != nil {
			e.attrs.Locked = false
		}
	}
	b.unlocked = append(b.unlocked, m.Ref())
	return nil
}

func (b *Backend) UnlockTag(_ context.Context, repository, tag string) error {
	if err := b.begin("UnlockTag", repository); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.lockedTags[repository], tag)
	b.unlocked = append(b.unlocked, repository+":"+tag)
	return nil
}

func (b *Backend) ProtectsLastTag() bool {
	return b.ProtectLastTag
}

// Image returns an image manifest whose config and layer digests derive from
// name, so that distinct names make distinct manifests.
func Image(name string) registry.OCIManifest {
	return registry.OCIManifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: godigest.FromString("config " + name), Size: 100},
		Layers:    []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip, Digest: godigest.FromString("layer " + name), Size: 1000}},
	}
}

// Index returns an image index referencing the given manifests.
func Index(children ...v1.Descriptor) registry.OCIManifest {
	return registry.OCIManifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: children,
	}
}

// Child returns the index entry for the manifest with the given digest,
// running on os/architecture.
func Child(digest, os, architecture string) v1.Descriptor {
	return v1.Descriptor{
		MediaType: v1.MediaTypeImageManifest,
		Digest:    godigest.Digest(digest),
		Platform:  &v1.Platform{OS: os, Architecture: architecture},
	}
}
