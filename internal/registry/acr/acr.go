// Package acr implements registry.Backend for Azure Container Registry, on top
// of the azcontainerregistry data-plane client.
package acr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"

	"github.com/JohanLindvall/crprune/internal/registry"
)

var (
	_ registry.Backend  = (*Backend)(nil)
	_ registry.Unlocker = (*Backend)(nil)
)

// Options configures a Backend.
type Options struct {
	// Endpoint is the registry's URL: https:// followed by its login server.
	Endpoint string
	// Credential authenticates to the registry, usually a credential from
	// azidentity (see CredentialOptions); nil makes anonymous requests.
	Credential azcore.TokenCredential
	// PageSize is the number of items per listing request.
	PageSize int
	// Logger receives warnings about retried requests.
	Logger *slog.Logger
}

// Backend speaks the ACR data-plane API. Its clients authenticate by
// challenge, requesting an access token scoped to exactly the repository and
// action each request needs, so it works on ABAC registries that grant
// permissions per repository.
type Backend struct {
	// An SDK client caches only the access token it obtained last, and ACR
	// issues tokens per repository and action, so a client serving requests
	// of alternating actions exchanges a token for nearly every one. Each
	// kind of request therefore has a client of its own: listings and
	// attribute reads (metadata_read), attribute updates (metadata_write),
	// and manifest downloads and deletions (pull, then delete — the two are
	// never interleaved).
	metadata, updates, content *azcontainerregistry.Client
	// first lets the first request of each action on a repository go alone,
	// so that the requests queued behind it reuse the token it obtains
	// instead of each exchanging one of their own.
	first    gates
	pageSize int32
}

// New returns a backend for the registry at opts.Endpoint. Throttled and
// transiently failing requests are retried, each retry logged as a warning:
// for as long as the registry asks, or with exponential backoff, up to
// minutes per request.
func New(opts Options) (*Backend, error) {
	return newBackend(opts, defaultRetry)
}

// newBackend returns a backend retrying requests as retry says.
func newBackend(opts Options, retry retryPolicy) (*Backend, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("a registry endpoint is required")
	}
	if opts.PageSize < 1 || opts.PageSize > math.MaxInt32 {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d", math.MaxInt32, opts.PageSize)
	}
	retry.logger = opts.Logger
	if retry.logger == nil {
		retry.logger = slog.New(slog.DiscardHandler)
	}
	b := &Backend{pageSize: int32(opts.PageSize)}
	transport := documentTransport{registry.NewHTTPClient(nil)}
	for _, client := range []**azcontainerregistry.Client{&b.metadata, &b.updates, &b.content} {
		c, err := azcontainerregistry.NewClient(opts.Endpoint, opts.Credential, &azcontainerregistry.ClientOptions{
			ClientOptions: azcore.ClientOptions{
				Transport: transport,
				Telemetry: policy.TelemetryOptions{ApplicationID: "crprune"},
				// retryPolicy replaces the SDK's retries, which are silent
				// and give up on throttling within seconds. The token
				// exchanges of the client's challenge authentication use
				// these options, and so the policy, as well.
				Retry:           policy.RetryOptions{MaxRetries: -1},
				PerCallPolicies: []policy.Policy{&retry},
			},
		})
		if err != nil {
			return nil, err
		}
		*client = c
	}
	return b, nil
}

// ListRepositories returns the names of all repositories in the registry.
func (b *Backend) ListRepositories(ctx context.Context) ([]string, error) {
	repositories := []string{}
	pager := b.metadata.NewListRepositoriesPager(&azcontainerregistry.ClientListRepositoriesOptions{MaxNum: to.Ptr(b.pageSize)})
	err := forEachPage(ctx, pager, func(page azcontainerregistry.ClientListRepositoriesResponse) *string { return page.Link }, func(page azcontainerregistry.ClientListRepositoriesResponse) error {
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
	pager := b.metadata.NewListManifestsPager(repository, &azcontainerregistry.ClientListManifestsOptions{
		OrderBy: to.Ptr(azcontainerregistry.ArtifactManifestOrderByNone),
		MaxNum:  to.Ptr(b.pageSize),
	})
	return forEachPage(ctx, pager, func(page azcontainerregistry.ClientListManifestsResponse) *string { return page.Link }, func(page azcontainerregistry.ClientListManifestsResponse) error {
		for _, attrs := range page.Attributes {
			if attrs != nil {
				for _, tag := range attrs.Tags {
					if tag == nil {
						return fmt.Errorf("manifest listing for %s contains a null tag", repository)
					}
				}
			}
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
	var res azcontainerregistry.ClientGetManifestResponse
	err := b.first.do(ctx, "pull", repository, func() (err error) {
		res, err = b.content.GetManifest(ctx, repository, digest, &azcontainerregistry.ClientGetManifestOptions{Accept: to.Ptr(registry.ManifestMediaTypes)})
		return err
	})
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { _ = res.ManifestData.Close() }()
	return registry.ReadDocument(res.ManifestData)
}

// DeleteManifest deletes a manifest and its tags.
func (b *Backend) DeleteManifest(ctx context.Context, m *registry.Manifest) error {
	return deleted(b.first.do(ctx, "delete", m.Repository, func() error {
		_, err := b.content.DeleteManifest(ctx, m.Repository, m.Digest, nil)
		return err
	}))
}

// DeleteRepository deletes a repository.
func (b *Backend) DeleteRepository(ctx context.Context, repository string) error {
	return deleted(b.first.do(ctx, "delete", repository, func() error {
		_, err := b.content.DeleteRepository(ctx, repository, nil)
		return err
	}))
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
	pager := b.metadata.NewListTagsPager(repository, &azcontainerregistry.ClientListTagsOptions{MaxNum: to.Ptr(b.pageSize)})
	err := forEachPage(ctx, pager, func(page azcontainerregistry.ClientListTagsResponse) *string { return page.Link }, func(page azcontainerregistry.ClientListTagsResponse) error {
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

// UnlockManifest re-enables delete and write on a manifest. It reads the
// manifest's attributes first, so that the Relock it returns restores exactly
// the ones it changed; it changes nothing on a manifest that is not locked, or
// no longer exists. Once it has read them, it returns the Relock even when the
// update fails, as the Unlocker interface asks.
func (b *Backend) UnlockManifest(ctx context.Context, m *registry.Manifest) (registry.Relock, error) {
	var props azcontainerregistry.ClientGetManifestPropertiesResponse
	err := b.first.do(ctx, "metadata_read", m.Repository, func() (err error) {
		props, err = b.metadata.GetManifestProperties(ctx, m.Repository, m.Digest, nil)
		return err
	})
	if err = wrap(err); registry.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var current azcontainerregistry.ManifestWriteableProperties
	if props.Manifest != nil && props.Manifest.ChangeableAttributes != nil {
		current = *props.Manifest.ChangeableAttributes
	}
	return unlock(ctx, current.CanDelete, current.CanWrite, func(ctx context.Context, canDelete, canWrite *bool) error {
		return b.first.do(ctx, "metadata_write", m.Repository, func() error {
			_, err := b.updates.UpdateManifestProperties(ctx, m.Repository, m.Digest, &azcontainerregistry.ClientUpdateManifestPropertiesOptions{
				Value: &azcontainerregistry.ManifestWriteableProperties{CanDelete: canDelete, CanWrite: canWrite},
			})
			return wrap(err)
		})
	})
}

// UnlockTag re-enables delete and write on a tag, as UnlockManifest does on a
// manifest.
func (b *Backend) UnlockTag(ctx context.Context, repository, tag string) (registry.Relock, error) {
	var props azcontainerregistry.ClientGetTagPropertiesResponse
	err := b.first.do(ctx, "metadata_read", repository, func() (err error) {
		props, err = b.metadata.GetTagProperties(ctx, repository, tag, nil)
		return err
	})
	if err = wrap(err); registry.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var current azcontainerregistry.TagWriteableProperties
	if props.Tag != nil && props.Tag.ChangeableAttributes != nil {
		current = *props.Tag.ChangeableAttributes
	}
	return unlock(ctx, current.CanDelete, current.CanWrite, func(ctx context.Context, canDelete, canWrite *bool) error {
		return b.first.do(ctx, "metadata_write", repository, func() error {
			_, err := b.updates.UpdateTagProperties(ctx, repository, tag, &azcontainerregistry.ClientUpdateTagPropertiesOptions{
				Value: &azcontainerregistry.TagWriteableProperties{CanDelete: canDelete, CanWrite: canWrite},
			})
			return wrap(err)
		})
	})
}

// unlock enables delete and write through update, given their current values,
// and returns the Relock setting back those it enabled — or nil, changing
// nothing, when neither is disabled. update leaves a nil flag unchanged.
//
// The Relock comes with the error of an update that failed, too: the
// registry may have applied an update whose response never arrived, cut off
// by an interruption, say. Setting back what was disabled anyway is harmless.
func unlock(ctx context.Context, canDelete, canWrite *bool, update func(ctx context.Context, canDelete, canWrite *bool) error) (registry.Relock, error) {
	if !locked(canDelete, canWrite) {
		return nil, nil
	}
	disabled := func(flag *bool) *bool {
		if flag != nil && !*flag {
			return to.Ptr(false)
		}
		return nil
	}
	restoreDelete, restoreWrite := disabled(canDelete), disabled(canWrite)
	restore := func(ctx context.Context) error {
		return update(ctx, restoreDelete, restoreWrite)
	}
	return restore, update(ctx, to.Ptr(true), to.Ptr(true))
}

// forEachPage advances the pager to exhaustion, invoking fn on every page.
// Errors from paging and from fn alike abort the walk. nextLink extracts the
// continuation URL so a broken server cannot loop forever; it may be nil for
// pagers without continuation URLs.
func forEachPage[T any](ctx context.Context, pager *runtime.Pager[T], nextLink func(T) *string, fn func(T) error) error {
	seen := map[string]bool{}
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return wrap(err)
		}
		if nextLink != nil {
			if link := nextLink(page); link != nil && *link != "" {
				if seen[*link] {
					return errors.New("pagination repeated a continuation URL")
				}
				seen[*link] = true
			}
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
