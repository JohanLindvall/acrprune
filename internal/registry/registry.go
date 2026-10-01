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
	"sync"

	"github.com/JohanLindvall/crprune/internal/imageref"

	"golang.org/x/sync/errgroup"
)

// Registry reads and deletes the manifests of one registry through its
// Backend, adding parallelism, caching, digest verification and logging.
type Registry struct {
	backend     Backend
	logger      *slog.Logger
	parallelism int
	cache       *Cache
	// cacheWarning logs only the first failure to write to the cache: one
	// failure typically means all of them fail.
	cacheWarning sync.Once
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

// pool returns an errgroup limited to the configured parallelism whose tasks
// do not cancel each other: only the caller's context stops them. Deletions
// use it, so that one failure does not abort unrelated requests midway.
func (r *Registry) pool() *errgroup.Group {
	var group errgroup.Group
	group.SetLimit(r.parallelism)
	return &group
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

// ProtectsLastTag reports whether the registry refuses to delete the last
// tagged manifest of a repository that is not deleted outright, so that
// deleting it would fail.
func (r *Registry) ProtectsLastTag() bool {
	protector, ok := r.backend.(LastTagProtector)
	return ok && protector.ProtectsLastTag()
}
