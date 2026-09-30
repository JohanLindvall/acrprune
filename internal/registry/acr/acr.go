// Package acr implements registry.Backend for Azure Container Registry, on top
// of the azcontainerregistry data-plane client.
package acr

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

var (
	_ registry.Backend  = (*Backend)(nil)
	_ registry.Unlocker = (*Backend)(nil)
)

// Backend speaks the ACR data-plane API. The client authenticates by
// challenge, requesting an access token scoped to exactly the repository each
// request touches, so it works on ABAC registries that grant permissions per
// repository.
type Backend struct {
	client   *azcontainerregistry.Client
	pageSize int32
}

// New returns a backend over client listing pageSize items per request, which
// must be positive: a zero page size asks the service for nothing.
func New(client *azcontainerregistry.Client, pageSize int) (*Backend, error) {
	if pageSize < 1 || pageSize > math.MaxInt32 {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d", math.MaxInt32, pageSize)
	}
	return &Backend{client: client, pageSize: int32(pageSize)}, nil
}

// ListRepositories returns the names of all repositories in the registry.
func (b *Backend) ListRepositories(ctx context.Context) ([]string, error) {
	repositories := []string{}
	pager := b.client.NewListRepositoriesPager(&azcontainerregistry.ClientListRepositoriesOptions{MaxNum: to.Ptr(b.pageSize)})
	err := forEachPage(ctx, pager, func(page azcontainerregistry.ClientListRepositoriesResponse) error {
		for _, repository := range page.Names {
			if repository != nil {
				repositories = append(repositories, *repository)
			}
		}
		return nil
	})
	if registry.IsPermissionError(err) {
		// On ABAC registries catalog listing needs its own role.
		return nil, fmt.Errorf("failed to list repositories (listing the catalog requires the Container Registry Repository Catalog Lister role): %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}
	return repositories, nil
}

// ListManifests calls fn with the attributes of every manifest in the
// repository.
func (b *Backend) ListManifests(ctx context.Context, repository string, fn func(registry.Attributes) error) error {
	pager := b.client.NewListManifestsPager(repository, &azcontainerregistry.ClientListManifestsOptions{
		OrderBy: to.Ptr(azcontainerregistry.ArtifactManifestOrderByNone),
		MaxNum:  to.Ptr(b.pageSize),
	})
	return forEachPage(ctx, pager, func(page azcontainerregistry.ClientListManifestsResponse) error {
		for _, attrs := range page.Attributes {
			if err := fn(attributes(attrs)); err != nil {
				return err
			}
		}
		return nil
	})
}

// attributes converts ACR's manifest attributes. A nil entry yields no digest,
// which the caller rejects.
func attributes(a *azcontainerregistry.ManifestAttributes) registry.Attributes {
	var result registry.Attributes
	if a == nil {
		return result
	}
	if a.Digest != nil {
		result.Digest = *a.Digest
	}
	for _, tag := range a.Tags {
		if tag != nil {
			result.Tags = append(result.Tags, *tag)
		}
	}
	if a.LastUpdatedOn != nil {
		result.LastUpdated = *a.LastUpdatedOn
	}
	if a.Architecture != nil {
		result.Architecture = string(*a.Architecture)
	}
	if a.OperatingSystem != nil {
		result.OS = string(*a.OperatingSystem)
	}
	if c := a.ChangeableAttributes; c != nil {
		result.Locked = locked(c.CanDelete, c.CanWrite)
	}
	return result
}

// locked reports whether a set of changeable attributes protects an artifact
// from deletion. Manifests and tags carry the same flags in distinct types.
func locked(canDelete, canWrite *bool) bool {
	return canDelete != nil && !*canDelete || canWrite != nil && !*canWrite
}

// GetManifest downloads a manifest document.
func (b *Backend) GetManifest(ctx context.Context, repository, digest string) ([]byte, error) {
	res, err := b.client.GetManifest(ctx, repository, digest, &azcontainerregistry.ClientGetManifestOptions{Accept: to.Ptr(registry.ManifestMediaTypes)})
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { _ = res.ManifestData.Close() }()
	return registry.ReadDocument(res.ManifestData)
}

// DeleteManifest deletes a manifest and its tags.
func (b *Backend) DeleteManifest(ctx context.Context, m *registry.Manifest) error {
	_, err := b.client.DeleteManifest(ctx, m.Repository, m.Digest, nil)
	return deleted(err)
}

// DeleteRepository deletes a repository.
func (b *Backend) DeleteRepository(ctx context.Context, repository string) error {
	_, err := b.client.DeleteRepository(ctx, repository, nil)
	return deleted(err)
}

// A concurrently removed resource already satisfies the delete request.
func deleted(err error) error {
	err = wrap(err)
	if registry.IsNotFound(err) {
		return nil
	}
	return err
}

// LockedTags returns the names of the repository's tags whose delete or write
// attribute is disabled.
func (b *Backend) LockedTags(ctx context.Context, repository string) (map[string]bool, error) {
	lockedTags := map[string]bool{}
	pager := b.client.NewListTagsPager(repository, &azcontainerregistry.ClientListTagsOptions{MaxNum: to.Ptr(b.pageSize)})
	err := forEachPage(ctx, pager, func(page azcontainerregistry.ClientListTagsResponse) error {
		for _, t := range page.Tags {
			if t == nil || t.Name == nil || t.ChangeableAttributes == nil {
				continue
			}
			if c := t.ChangeableAttributes; locked(c.CanDelete, c.CanWrite) {
				lockedTags[*t.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lockedTags, nil
}

// UnlockManifest re-enables delete and write on a manifest.
func (b *Backend) UnlockManifest(ctx context.Context, m *registry.Manifest) error {
	_, err := b.client.UpdateManifestProperties(ctx, m.Repository, m.Digest, &azcontainerregistry.ClientUpdateManifestPropertiesOptions{
		Value: &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)},
	})
	return wrap(err)
}

// UnlockTag re-enables delete and write on a tag.
func (b *Backend) UnlockTag(ctx context.Context, repository, tag string) error {
	_, err := b.client.UpdateTagProperties(ctx, repository, tag, &azcontainerregistry.ClientUpdateTagPropertiesOptions{
		Value: &azcontainerregistry.TagWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)},
	})
	return wrap(err)
}

// forEachPage advances the pager to exhaustion, invoking fn on every page.
// Errors from paging and from fn alike abort the walk.
func forEachPage[T any](ctx context.Context, pager *runtime.Pager[T], fn func(T) error) error {
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return wrap(err)
		}
		if err := fn(page); err != nil {
			return err
		}
	}
	return nil
}

// wrap turns the SDK's response errors into registry.ResponseError, keeping
// the original in the chain for its detailed message.
func wrap(err error) error {
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		return &registry.ResponseError{StatusCode: responseErr.StatusCode, Err: err}
	}
	return err
}
