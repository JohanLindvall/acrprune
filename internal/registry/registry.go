// Package registry is the registry-neutral side of registry access: the
// manifest model, parallel manifest download with digest verification and
// on-disk caching, and deletion, on top of a Backend speaking one registry's
// API (see the acr and ghcr packages).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/JohanLindvall/acrprune/internal/imageref"

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
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repositories, err := r.backend.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	// Stable ordering makes cross-repository byte attribution reproducible.
	slices.Sort(repositories)
	repositories = slices.Compact(repositories)
	for _, repository := range repositories {
		if !imageref.ValidRepository(repository) {
			return nil, fmt.Errorf("listing returned an invalid repository name %q", repository)
		}
	}
	r.logger.Debug("Fetched repositories", "count", len(repositories))
	return repositories, nil
}

// DeleteRepository deletes an entire repository and evicts its known manifests
// from the cache. Pass the inspected manifests when they are available.
func (r *Registry) DeleteRepository(ctx context.Context, repository string, known ...*Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !imageref.ValidRepository(repository) {
		return fmt.Errorf("invalid repository name %q", repository)
	}
	if err := r.backend.DeleteRepository(ctx, repository); err != nil {
		return fmt.Errorf("failed to delete repository %s: %w", repository, err)
	}
	for _, m := range known {
		r.cache.Remove(m.Digest)
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
