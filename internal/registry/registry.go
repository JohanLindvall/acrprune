// Package registry wraps the ACR data-plane client with paging, parallel
// manifest download and optional on-disk caching.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	azerrors "github.com/Azure/azure-sdk-for-go-extensions/pkg/errors"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"golang.org/x/sync/errgroup"
)

type Registry struct {
	client      *azcontainerregistry.Client
	logger      *slog.Logger
	pageSize    int32
	parallelism int
	cache       *Cache
}

func New(client *azcontainerregistry.Client, logger *slog.Logger, pageSize, parallelism int, cache *Cache) *Registry {
	return &Registry{
		client:      client,
		logger:      logger,
		pageSize:    int32(pageSize),
		parallelism: parallelism,
		cache:       cache,
	}
}

// ListRepositories returns the names of all repositories in the registry.
func (r *Registry) ListRepositories(ctx context.Context) ([]string, error) {
	repositories := []string{}
	pager := r.client.NewListRepositoriesPager(&azcontainerregistry.ClientListRepositoriesOptions{MaxNum: to.Ptr(r.pageSize)})
	page := 0
	for pager.More() {
		r.logger.Debug("Fetching ACR repositories", "page", page)
		page++
		repositoryPage, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to advance repository page: %w", err)
		}
		for _, repository := range repositoryPage.Names {
			repositories = append(repositories, *repository)
		}
	}

	r.logger.Debug("Fetched repositories", "count", len(repositories))

	return repositories, nil
}

// FetchRepositoryManifests downloads every manifest in the repository, keyed
// by repository@digest. found is false when the repository itself does not
// exist and ignoreMissing is set.
func (r *Registry) FetchRepositoryManifests(ctx context.Context, repository string, ignoreMissing bool) (manifests map[string]*Manifest, found bool, err error) {
	pager := r.client.NewListManifestsPager(repository, &azcontainerregistry.ClientListManifestsOptions{
		OrderBy: to.Ptr(azcontainerregistry.ArtifactManifestOrderByNone),
		MaxNum:  to.Ptr(r.pageSize),
	})

	var mu sync.Mutex
	manifests = map[string]*Manifest{}
	attributes := map[string]*azcontainerregistry.ManifestAttributes{}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.parallelism)

	page := 0
	for pager.More() {
		r.logger.Debug("Fetching manifest attributes", "page", page, "repository", repository)
		page++
		attributePage, pageErr := pager.NextPage(ctx)
		if pageErr != nil {
			_ = group.Wait()
			if re := azerrors.IsResponseError(pageErr); re != nil && re.StatusCode == 404 && ignoreMissing {
				r.logger.Warn("Repository missing", "repository", repository)
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("failed to list manifests for %s: %w", repository, pageErr)
		}
		for _, attrs := range attributePage.Attributes {
			ref := MakeRef(repository, *attrs.Digest)
			attributes[ref] = attrs
			group.Go(func() error {
				m, err := r.fetchManifest(groupCtx, ref, ignoreMissing)
				if err != nil {
					return err
				}
				if m != nil {
					mu.Lock()
					manifests[m.Ref] = m
					mu.Unlock()
				}
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, false, err
	}

	for ref, attrs := range attributes {
		if m, ok := manifests[ref]; ok {
			m.Azure = attrs
		}
	}

	return manifests, true, nil
}

// fetchManifest downloads (or loads from cache) a single manifest document.
// It returns nil without error when the manifest is missing and
// ignoreMissing is set.
func (r *Registry) fetchManifest(ctx context.Context, ref string, ignoreMissing bool) (*Manifest, error) {
	repository, digest := ParseRef(ref)
	raw := r.cache.Get(digest)

	if raw == nil {
		accept := fmt.Sprintf("application/vnd.oci.image.index.v1+json,%s,%s", azcontainerregistry.ContentTypeApplicationVndDockerDistributionManifestV2JSON, azcontainerregistry.ContentTypeApplicationVndOciImageManifestV1JSON)
		r.logger.Debug("Downloading manifest", "manifest", ref)
		res, err := r.client.GetManifest(ctx, repository, digest, &azcontainerregistry.ClientGetManifestOptions{Accept: &accept})
		if err != nil {
			if re := azerrors.IsResponseError(err); re != nil && re.StatusCode == 404 && ignoreMissing {
				r.logger.Warn("Manifest missing", "manifest", ref)
				return nil, nil
			}
			return nil, fmt.Errorf("failed to get manifest %s: %w", ref, err)
		}
		reader, err := azcontainerregistry.NewDigestValidationReader(*res.DockerContentDigest, res.ManifestData)
		if err != nil {
			return nil, err
		}
		if raw, err = io.ReadAll(reader); err != nil {
			return nil, err
		}
		r.cache.Put(digest, raw)
	}

	result := &Manifest{Ref: ref, Size: uint64(len(raw))}
	if err := json.Unmarshal(raw, &result.OCIManifest); err != nil {
		return nil, fmt.Errorf("failed to decode manifest %s: %w", ref, err)
	}
	if result.SchemaVersion != 2 {
		return nil, fmt.Errorf("manifest %s: unsupported schema version %d", ref, result.SchemaVersion)
	}
	if result.Config != nil {
		result.Size += uint64(result.Config.Size)
	}
	return result, nil
}

// DeleteManifests deletes the given manifests in parallel, evicting each
// from the cache.
func (r *Registry) DeleteManifests(ctx context.Context, manifests []*Manifest) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.parallelism)
	for _, m := range manifests {
		group.Go(func() error {
			r.logger.Info("Deleting manifest", "manifest", m)
			repository, digest := ParseRef(m.Ref)
			if _, err := r.client.DeleteManifest(groupCtx, repository, digest, nil); err != nil {
				return fmt.Errorf("failed to delete manifest %s: %w", m.Ref, err)
			}
			r.cache.Remove(digest)
			return nil
		})
	}
	return group.Wait()
}

// ListTagLocks returns the set of tag names in the repository whose delete or
// write attribute is disabled. It is used before deletion, when unlocking is
// requested, so that only genuinely locked tags are updated.
func (r *Registry) ListTagLocks(ctx context.Context, repository string) (map[string]bool, error) {
	locked := map[string]bool{}
	pager := r.client.NewListTagsPager(repository, &azcontainerregistry.ClientListTagsOptions{MaxNum: to.Ptr(r.pageSize)})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list tags for %s: %w", repository, err)
		}
		for _, t := range page.Tags {
			c := t.ChangeableAttributes
			if c != nil && ((c.CanDelete != nil && !*c.CanDelete) || (c.CanWrite != nil && !*c.CanWrite)) {
				locked[*t.Name] = true
			}
		}
	}
	return locked, nil
}

// UnlockManifests re-enables delete and write on the locked manifests and on
// any of their tags named in lockedTags, so a subsequent delete can proceed.
// Individual failures are logged and tolerated: the delete that follows
// surfaces anything that genuinely could not be unlocked.
func (r *Registry) UnlockManifests(ctx context.Context, manifests []*Manifest, lockedTags map[string]bool) {
	unlocked := &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)}
	unlockedTag := &azcontainerregistry.TagWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.parallelism)
	for _, m := range manifests {
		repository, digest := ParseRef(m.Ref)
		if m.Locked() {
			group.Go(func() error {
				r.logger.Info("Unlocking manifest", "manifest", m)
				if _, err := r.client.UpdateManifestProperties(groupCtx, repository, digest, &azcontainerregistry.ClientUpdateManifestPropertiesOptions{Value: unlocked}); err != nil {
					r.logger.Warn("Failed to unlock manifest; attempting deletion anyway", "manifest", m.Ref, "err", err)
				}
				return nil
			})
		}
		for _, tag := range m.Tags() {
			if !lockedTags[tag] {
				continue
			}
			group.Go(func() error {
				r.logger.Info("Unlocking tag", "repository", repository, "tag", tag)
				if _, err := r.client.UpdateTagProperties(groupCtx, repository, tag, &azcontainerregistry.ClientUpdateTagPropertiesOptions{Value: unlockedTag}); err != nil {
					r.logger.Warn("Failed to unlock tag; attempting deletion anyway", "repository", repository, "tag", tag, "err", err)
				}
				return nil
			})
		}
	}
	_ = group.Wait()
}

// DeleteRepository deletes an entire repository.
func (r *Registry) DeleteRepository(ctx context.Context, repository string) error {
	if _, err := r.client.DeleteRepository(ctx, repository, nil); err != nil {
		return fmt.Errorf("failed to delete repository %s: %w", repository, err)
	}
	return nil
}
