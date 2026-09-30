package registry

import (
	"context"
	"fmt"
	"slices"

	"github.com/JohanLindvall/acrprune/internal/imageref"
	"github.com/JohanLindvall/acrprune/internal/progress"
	"github.com/opencontainers/go-digest"
)

// VerifyRepositorySnapshot rechecks attributes before a whole-repository
// delete, which would otherwise also remove images pushed during inspection.
// Registries provide no atomic compare-and-delete; writers must still avoid
// changing the repository between this check and deletion.
func (r *Registry) VerifyRepositorySnapshot(ctx context.Context, repository string, expected []*Manifest) error {
	remaining := make(map[string]Attributes, len(expected))
	for _, m := range expected {
		remaining[m.Digest] = m.Attributes
	}
	changed := func() error {
		return fmt.Errorf("repository %s changed during inspection; refusing to delete it, rerun the command", repository)
	}
	err := r.backend.ListManifests(ctx, repository, func(got Attributes) error {
		want, exists := remaining[got.Digest]
		if !exists || !got.LastUpdated.Equal(want.LastUpdated) || got.ID != want.ID || got.Locked != want.Locked {
			return changed()
		}
		a, b := slices.Clone(got.Tags), slices.Clone(want.Tags)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			return changed()
		}
		delete(remaining, got.Digest)
		return ctx.Err()
	})
	if IsNotFound(err) {
		return nil // already deleted
	}
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return changed()
	}
	return ctx.Err()
}

// DeleteManifests deletes parents before their dependencies, with independent
// manifests deleted in parallel. A failed parent deletion leaves its children
// intact, including when a registry lock or permission prevents deletion.
func (r *Registry) DeleteManifests(ctx context.Context, manifests []*Manifest) error {
	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Deleting manifests"})
	batches, err := deletionBatches(manifests)
	if err != nil {
		return err
	}
	for _, batch := range batches {
		group, groupCtx := r.group(ctx)
		for _, m := range batch {
			group.Go(func() error {
				if err := groupCtx.Err(); err != nil {
					return err
				}
				r.logger.Info("Deleting manifest", "manifest", m)
				if err := r.backend.DeleteManifest(groupCtx, m); err != nil {
					return fmt.Errorf("failed to delete manifest %s: %w", m.Ref(), err)
				}
				r.cache.Remove(m.Digest)
				progress.Report(ctx, progress.Event{Kind: progress.Deleted, Count: 1})
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// deletionBatches validates the entire graph before any mutation, then
// topologically orders it in O(manifests + references) time.
func deletionBatches(manifests []*Manifest) ([][]*Manifest, error) {
	byRef := make(map[string]*Manifest, len(manifests))
	for _, m := range manifests {
		if m == nil || !imageref.ValidRepository(m.Repository) {
			return nil, fmt.Errorf("invalid manifest selected for deletion")
		}
		if _, err := digest.Parse(m.Digest); err != nil {
			return nil, fmt.Errorf("invalid manifest digest %q: %w", m.Digest, err)
		}
		if _, duplicate := byRef[m.Ref()]; duplicate {
			return nil, fmt.Errorf("manifest %s selected for deletion more than once", m.Ref())
		}
		byRef[m.Ref()] = m
	}
	parents := map[*Manifest]int{}
	children := map[*Manifest][]*Manifest{}
	for _, m := range manifests {
		add := func(digest string) {
			if child := byRef[m.Repository+"@"+digest]; child != nil {
				parents[child]++
				children[m] = append(children[m], child)
			}
		}
		for _, child := range m.Manifests {
			add(string(child.Digest))
		}
		if m.Subject != nil {
			add(string(m.Subject.Digest))
		}
	}
	var ready []*Manifest
	for _, m := range manifests {
		if parents[m] == 0 {
			ready = append(ready, m)
		}
	}
	var batches [][]*Manifest
	visited := 0
	for len(ready) > 0 {
		batches = append(batches, ready)
		visited += len(ready)
		var next []*Manifest
		for _, m := range ready {
			for _, child := range children[m] {
				parents[child]--
				if parents[child] == 0 {
					next = append(next, child)
				}
			}
		}
		ready = next
	}
	if visited != len(manifests) {
		return nil, fmt.Errorf("cannot delete a cyclic manifest dependency graph")
	}
	return batches, nil
}
