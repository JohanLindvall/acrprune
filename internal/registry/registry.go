// Package registry wraps the ACR data-plane client with paging, parallel
// manifest download and optional on-disk caching.
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"golang.org/x/sync/errgroup"
)

// LoginServer returns the registry's login server host name: the name itself
// when it already contains a dot (a full login server, e.g. a sovereign
// cloud's), otherwise the public-cloud host <name>.azurecr.io.
func LoginServer(name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	return name + ".azurecr.io"
}

// forEachPage advances the pager to exhaustion, invoking fn on every page.
// Errors from paging and from fn alike abort the walk.
func forEachPage[T any](ctx context.Context, pager *runtime.Pager[T], fn func(T) error) error {
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
	}
	return nil
}

type Registry struct {
	client      *azcontainerregistry.Client
	logger      *slog.Logger
	pageSize    int32
	parallelism int
	cache       *Cache
}

// New returns a Registry reading through the given cache, which may be nil.
// pageSize and parallelism must be positive: a zero page size asks the service
// for nothing, and a zero parallelism limit stalls every download group
// forever.
func New(client *azcontainerregistry.Client, logger *slog.Logger, pageSize, parallelism int, cache *Cache) (*Registry, error) {
	if pageSize < 1 || pageSize > math.MaxInt32 {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d", math.MaxInt32, pageSize)
	}
	if parallelism < 1 {
		return nil, fmt.Errorf("parallelism must be at least 1, got %d", parallelism)
	}
	return &Registry{
		client:      client,
		logger:      logger,
		pageSize:    int32(pageSize),
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
	repositories := []string{}
	pager := r.client.NewListRepositoriesPager(&azcontainerregistry.ClientListRepositoriesOptions{MaxNum: to.Ptr(r.pageSize)})
	err := forEachPage(ctx, pager, func(page azcontainerregistry.ClientListRepositoriesResponse) error {
		r.logger.Debug("Fetching ACR repositories", "count", len(repositories))
		for _, repository := range page.Names {
			if repository != nil {
				repositories = append(repositories, *repository)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to advance repository page: %w", err)
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
			if _, err := r.client.DeleteManifest(groupCtx, m.Repository, m.Digest, nil); err != nil {
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
	if _, err := r.client.DeleteRepository(ctx, repository, nil); err != nil {
		return fmt.Errorf("failed to delete repository %s: %w", repository, err)
	}
	return nil
}

// IsPermissionError reports whether err is an ACR 401/403, i.e. the caller
// lacks permission on the resource (typical on ABAC registries scoped to a
// subset of repositories).
func IsPermissionError(err error) bool {
	return hasStatus(err, http.StatusUnauthorized) || hasStatus(err, http.StatusForbidden)
}

// hasStatus reports whether err is, or wraps, an HTTP response error carrying
// the given status code.
func hasStatus(err error, status int) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}
