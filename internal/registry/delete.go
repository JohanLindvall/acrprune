package registry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/imageref"
	"github.com/JohanLindvall/acrprune/internal/progress"
)

// DeleteOptions tunes DeleteManifests and DeleteRepository.
type DeleteOptions struct {
	// Unlock re-enables delete and write on every locked manifest, and on its
	// LockedTags, immediately before deleting it, on registries with locks.
	// When the deletion then fails, the locks are restored as far as
	// possible. Load the tag locks with LoadTagLocks first.
	Unlock bool
}

// VerifyRepositorySnapshot rechecks attributes before deleting from a
// repository: a whole-repository delete would otherwise also remove images
// pushed during inspection, and a decision made from a listing that missed a
// manifest — an index, say, whose images then look unreferenced — is unsafe.
// It fails with ErrRepositoryChanged when a manifest was added, removed,
// retagged, updated, replaced, locked or unlocked since expected was listed —
// tag locks included, for the manifests LoadTagLocks loaded them for — and with
// ErrRepositoryGone when the repository no longer exists. Registries provide
// no atomic compare-and-delete; writers must still avoid changing the
// repository between this check and deletion.
func (r *Registry) VerifyRepositorySnapshot(ctx context.Context, repository string, expected []*Manifest) error {
	byDigest := make(map[string]*Manifest, len(expected))
	for _, m := range expected {
		byDigest[m.Digest] = m
	}
	remaining := maps.Clone(byDigest)
	var change error
	changed := func(format string, args ...any) error {
		change = changedError(repository, fmt.Sprintf(format, args...))
		return change
	}
	err := r.backend.ListManifests(ctx, repository, func(got Attributes) error {
		want, known := byDigest[got.Digest]
		if !known {
			return changed("manifest %s@%s is new", repository, got.Digest)
		}
		if _, pending := remaining[got.Digest]; !pending {
			return changed("manifest %s was listed more than once", want.Ref())
		}
		if how := attributeChange(want.Attributes, got); how != "" {
			return changed("manifest %s %s", want.Ref(), how)
		}
		delete(remaining, got.Digest)
		return ctx.Err()
	})
	if change != nil {
		return change // however the backend wrapped it
	}
	if IsNotFound(err) {
		return goneError(repository, err)
	}
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return changed("manifest %s is gone", remaining[slices.Min(slices.Collect(maps.Keys(remaining)))].Ref())
	}
	if err := r.verifyTagLocks(ctx, repository, expected); err != nil {
		return err
	}
	return ctx.Err()
}

// attributeChange describes how a manifest's listed attributes differ from the
// ones it was inspected with, or returns "" when they do not.
func attributeChange(want, got Attributes) string {
	switch {
	case !got.LastUpdated.Equal(want.LastUpdated):
		return "was updated"
	case got.ID != want.ID:
		return "was replaced"
	case got.Locked != want.Locked:
		return "was locked or unlocked"
	case !sameSet(got.Tags, want.Tags):
		return "was retagged"
	}
	return ""
}

// verifyTagLocks compares the tag locks the registry reports with those of the
// expected manifests LoadTagLocks loaded them for.
func (r *Registry) verifyTagLocks(ctx context.Context, repository string, expected []*Manifest) error {
	unlocker, ok := r.backend.(Unlocker)
	if !ok || !slices.ContainsFunc(expected, func(m *Manifest) bool { return m.LockedTags != nil }) {
		return nil
	}
	locked, err := unlocker.LockedTags(ctx, repository)
	if IsNotFound(err) {
		return goneError(repository, err)
	}
	if err != nil {
		return fmt.Errorf("failed to list tags for %s: %w", repository, err)
	}
	for _, m := range expected {
		if m.LockedTags != nil && !sameSet(lockedTagsOf(m.Tags, locked), m.LockedTags) {
			return changedError(repository, fmt.Sprintf("the tag locks of manifest %s changed", m.Ref()))
		}
	}
	return nil
}

// changedError reports that the snapshot of repository is stale, and why.
func changedError(repository, why string) error {
	return fmt.Errorf("%w: %s; refusing to delete from repository %s, rerun the command", ErrRepositoryChanged, why, repository)
}

// goneError reports that repository no longer exists, as the not-found
// response err says.
func goneError(repository string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrRepositoryGone, repository, err)
}

// sameSet reports whether a and b hold the same strings, in any order.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// DeleteManifests deletes parents before their dependencies — an index before
// the manifests it references, and a subject before its referrers — with
// independent manifests deleted in parallel. A failed deletion leaves the
// manifests depending on it intact, and in turn theirs: the children of an
// index that a lock or missing permission keeps, and the signatures of a kept
// image. It does not stop anything else: in-flight requests complete, and
// every independent manifest is still deleted. Only ctx cancels the rest. The
// failures are returned joined, so IsPermissionError and IsNotFound see
// through to them.
func (r *Registry) DeleteManifests(ctx context.Context, manifests []*Manifest, opts DeleteOptions) error {
	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Deleting manifests"})
	batches, parents, err := deletionBatches(manifests)
	if err != nil {
		return err
	}
	var unlocker Unlocker
	if opts.Unlock {
		unlocker, _ = r.backend.(Unlocker)
	}
	var mu sync.Mutex
	var failures []error
	notDeleted := map[*Manifest]bool{}
	for _, batch := range batches {
		if ctx.Err() != nil {
			break
		}
		group := r.pool()
		for _, m := range batch {
			mu.Lock()
			blocker := slices.IndexFunc(parents[m], func(parent *Manifest) bool { return notDeleted[parent] })
			if blocker >= 0 {
				notDeleted[m] = true
			}
			mu.Unlock()
			if blocker >= 0 {
				r.logger.Warn("Keeping manifest: its index or subject could not be deleted", "manifest", m, "blocked_by", parents[m][blocker].Ref())
				continue
			}
			group.Go(func() error {
				err := r.deleteManifest(ctx, unlocker, m)
				if err != nil {
					mu.Lock()
					defer mu.Unlock()
					notDeleted[m] = true
					// The interruption, returned below, is the only
					// failure worth reporting of deletions it cut short;
					// a refusal that came first is reported all the same.
					if ctx.Err() == nil || !canceled(err) {
						failures = append(failures, err)
					}
				}
				return nil
			})
		}
		_ = group.Wait()
	}
	return errors.Join(append(failures, ctx.Err())...)
}

// canceled reports whether err is a cancellation's, or a deadline's.
func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// deleteManifest deletes one manifest, first unlocking it when unlocker is set
// and relocking it when the deletion fails.
func (r *Registry) deleteManifest(ctx context.Context, unlocker Unlocker, m *Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var relocks []relock
	if unlocker != nil {
		relocks = r.unlock(ctx, unlocker, m)
	}
	r.logger.Info("Deleting manifest", "manifest", m)
	if err := r.backend.DeleteManifest(ctx, m); err != nil {
		r.relock(ctx, relocks)
		return fmt.Errorf("failed to delete manifest %s: %w", m.Ref(), err)
	}
	r.cache.Remove(m.Digest)
	progress.Report(ctx, progress.Event{Kind: progress.Deleted, Count: 1})
	return nil
}

// DeleteRepository deletes an entire repository and evicts its known manifests
// from the cache. Pass the inspected manifests when they are available; with
// opts.Unlock, their locks are removed just before the deletion and restored
// if it fails.
func (r *Registry) DeleteRepository(ctx context.Context, repository string, known []*Manifest, opts DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !imageref.ValidRepository(repository) {
		return fmt.Errorf("invalid repository name %q", repository)
	}
	var relocks []relock
	if unlocker, ok := r.backend.(Unlocker); ok && opts.Unlock {
		relocks = r.unlockAll(ctx, unlocker, known)
	}
	err := ctx.Err() // canceled while unlocking
	if err == nil {
		err = r.backend.DeleteRepository(ctx, repository)
	}
	if err != nil {
		r.relock(ctx, relocks)
		return fmt.Errorf("failed to delete repository %s: %w", repository, err)
	}
	for _, m := range known {
		r.cache.Remove(m.Digest)
	}
	return nil
}

// deletionBatches validates the entire graph before any mutation, then
// topologically orders it in O(manifests + references) time. It also returns,
// for each manifest, the manifests that must be deleted before it: the indexes
// referencing it, and its subject.
func deletionBatches(manifests []*Manifest) (batches [][]*Manifest, parents map[*Manifest][]*Manifest, err error) {
	byRef := make(map[string]*Manifest, len(manifests))
	for _, m := range manifests {
		if m == nil || !imageref.ValidRepository(m.Repository) {
			return nil, nil, fmt.Errorf("invalid manifest selected for deletion")
		}
		if _, err := digest.Parse(m.Digest); err != nil {
			return nil, nil, fmt.Errorf("invalid manifest digest %q: %w", m.Digest, err)
		}
		if _, duplicate := byRef[m.Ref()]; duplicate {
			return nil, nil, fmt.Errorf("manifest %s selected for deletion more than once", m.Ref())
		}
		byRef[m.Ref()] = m
	}
	parents = map[*Manifest][]*Manifest{}
	children := map[*Manifest][]*Manifest{}
	// edge orders parent before child, when both are being deleted.
	edge := func(parent, child *Manifest) {
		if parent != nil && child != nil {
			parents[child] = append(parents[child], parent)
			children[parent] = append(children[parent], child)
		}
	}
	for _, m := range manifests {
		for _, desc := range m.Manifests {
			edge(m, byRef[m.Repository+"@"+string(desc.Digest)])
		}
		// A subject goes before its referrers, so that a subject that
		// cannot be deleted keeps its signatures: a signature left behind
		// is harmless garbage, an image without one can fail admission. If
		// a failure leaves a referrer behind once its subject is gone, one
		// naming the subject in its subject field goes on the next run,
		// while one naming it by tag becomes an ordinary tagged manifest,
		// which only a rule matching its tag deletes. An index that also
		// references its subject goes first, as an index.
		subject := m.SubjectDigest()
		if subject != "" && !slices.ContainsFunc(m.Manifests, func(desc v1.Descriptor) bool { return string(desc.Digest) == subject }) {
			edge(byRef[m.Repository+"@"+subject], m)
		}
	}
	pending := make(map[*Manifest]int, len(manifests))
	var ready []*Manifest
	for _, m := range manifests {
		pending[m] = len(parents[m])
		if pending[m] == 0 {
			ready = append(ready, m)
		}
	}
	visited := 0
	for len(ready) > 0 {
		batches = append(batches, ready)
		visited += len(ready)
		var next []*Manifest
		for _, m := range ready {
			for _, child := range children[m] {
				pending[child]--
				if pending[child] == 0 {
					next = append(next, child)
				}
			}
		}
		ready = next
	}
	if visited != len(manifests) {
		return nil, nil, fmt.Errorf("cannot delete a cyclic manifest dependency graph")
	}
	return batches, parents, nil
}
