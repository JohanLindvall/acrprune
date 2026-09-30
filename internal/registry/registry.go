// Package registry is the registry-neutral side of registry access: the
// manifest model, parallel manifest download with digest verification and
// on-disk caching, and deletion, on top of a Backend speaking one registry's
// API (see the acr and ghcr packages).
package registry

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

type Registry struct {
	backend     Backend
	logger      *slog.Logger
	parallelism int
	cache       *Cache
}

// New returns a Registry over the backend, reading through the given cache,
// which may be nil. parallelism bounds concurrent requests and must be
// positive: a zero limit stalls every download group forever.
func New(backend Backend, logger *slog.Logger, parallelism int, cache *Cache) (*Registry, error) {
	if parallelism < 1 {
		return nil, fmt.Errorf("parallelism must be at least 1, got %d", parallelism)
	}
	return &Registry{
		backend:     backend,
		logger:      logger,
		parallelism: parallelism,
		cache:       cache,
	}, nil
}

// Logger returns the logger the registry was built with, for callers that
// iterate over registry data and want consistent log output.
func (r *Registry) Logger() *slog.Logger {
	return r.logger
}

// group returns an errgroup limited to the configured parallelism, along with
// the context its tasks must use so a failure cancels its siblings.
func (r *Registry) group(ctx context.Context) (*errgroup.Group, context.Context) {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.parallelism)
	return group, groupCtx
}

// ListRepositories returns the names of all repositories in the registry.
func (r *Registry) ListRepositories(ctx context.Context) ([]string, error) {
	repositories, err := r.backend.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	r.logger.Debug("Fetched repositories", "count", len(repositories))
	return repositories, nil
}

// DeleteManifests deletes the given manifests in parallel, evicting each from
// the cache.
func (r *Registry) DeleteManifests(ctx context.Context, manifests []*Manifest) error {
	group, groupCtx := r.group(ctx)
	for _, m := range manifests {
		group.Go(func() error {
			r.logger.Info("Deleting manifest", "manifest", m)
			if err := r.backend.DeleteManifest(groupCtx, m); err != nil {
				return fmt.Errorf("failed to delete manifest %s: %w", m.Ref(), err)
			}
			r.cache.Remove(m.Digest)
			return nil
		})
	}
	return group.Wait()
}

// DeleteRepository deletes an entire repository.
func (r *Registry) DeleteRepository(ctx context.Context, repository string) error {
	if err := r.backend.DeleteRepository(ctx, repository); err != nil {
		return fmt.Errorf("failed to delete repository %s: %w", repository, err)
	}
	return nil
}

// ProtectsLastTag reports whether the registry refuses to delete the last
// tagged manifest of a repository that is not deleted outright, so that
// deleting it would fail.
func (r *Registry) ProtectsLastTag() bool {
	protector, ok := r.backend.(LastTagProtector)
	return ok && protector.ProtectsLastTag()
}
