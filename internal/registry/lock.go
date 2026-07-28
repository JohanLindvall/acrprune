package registry

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
)

// ListTagLocks returns the set of tag names in the repository whose delete or
// write attribute is disabled. It is used before deletion, when unlocking is
// requested, so that only genuinely locked tags are updated.
func (r *Registry) ListTagLocks(ctx context.Context, repository string) (map[string]bool, error) {
	lockedTags := map[string]bool{}
	pager := r.client.NewListTagsPager(repository, &azcontainerregistry.ClientListTagsOptions{MaxNum: to.Ptr(r.pageSize)})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list tags for %s: %w", repository, err)
		}
		for _, t := range page.Tags {
			c := t.ChangeableAttributes
			if t.Name != nil && c != nil && locked(c.CanDelete, c.CanWrite) {
				lockedTags[*t.Name] = true
			}
		}
	}
	return lockedTags, nil
}

// UnlockManifests re-enables delete and write on the locked manifests and on
// any of their tags named in lockedTags, so a subsequent delete can proceed.
// Individual failures are logged and tolerated: the delete that follows
// surfaces anything that genuinely could not be unlocked.
func (r *Registry) UnlockManifests(ctx context.Context, manifests []*Manifest, lockedTags map[string]bool) {
	unlocked := &azcontainerregistry.ManifestWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)}
	unlockedTag := &azcontainerregistry.TagWriteableProperties{CanDelete: to.Ptr(true), CanWrite: to.Ptr(true)}

	group, groupCtx := r.group(ctx)
	for _, m := range manifests {
		if m.Locked() {
			group.Go(func() error {
				r.logger.Info("Unlocking manifest", "manifest", m)
				if _, err := r.client.UpdateManifestProperties(groupCtx, m.Repository, m.Digest, &azcontainerregistry.ClientUpdateManifestPropertiesOptions{Value: unlocked}); err != nil {
					r.logger.Warn("Failed to unlock manifest; attempting deletion anyway", "manifest", m.Ref(), "err", err)
				}
				return nil
			})
		}
		for _, tag := range m.Tags() {
			if !lockedTags[tag] {
				continue
			}
			group.Go(func() error {
				r.logger.Info("Unlocking tag", "repository", m.Repository, "tag", tag)
				if _, err := r.client.UpdateTagProperties(groupCtx, m.Repository, tag, &azcontainerregistry.ClientUpdateTagPropertiesOptions{Value: unlockedTag}); err != nil {
					r.logger.Warn("Failed to unlock tag; attempting deletion anyway", "repository", m.Repository, "tag", tag, "err", err)
				}
				return nil
			})
		}
	}
	_ = group.Wait()
}
