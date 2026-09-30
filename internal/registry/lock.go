package registry

import (
	"context"
	"fmt"
)

// UnlockManifests re-enables delete and write on the locked manifests of the
// repository and on any locked tags pointing at them, so a subsequent delete
// can proceed. It does nothing on registries without locks. Individual
// failures are logged and tolerated: the delete that follows surfaces anything
// that genuinely could not be unlocked.
func (r *Registry) UnlockManifests(ctx context.Context, repository string, manifests []*Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlocker, ok := r.backend.(Unlocker)
	if !ok || len(manifests) == 0 {
		return nil
	}
	// Only tags that are genuinely locked are updated.
	lockedTags, err := unlocker.LockedTags(ctx, repository)
	if err != nil {
		return fmt.Errorf("failed to list tags for %s: %w", repository, err)
	}

	group, groupCtx := r.group(ctx)
	for _, m := range manifests {
		if m.Locked {
			group.Go(func() error {
				r.logger.Info("Unlocking manifest", "manifest", m)
				if err := unlocker.UnlockManifest(groupCtx, m); err != nil {
					r.logger.Warn("Failed to unlock manifest; attempting deletion anyway", "manifest", m.Ref(), "err", err)
				}
				return nil
			})
		}
		for _, tag := range m.Tags {
			if !lockedTags[tag] {
				continue
			}
			group.Go(func() error {
				r.logger.Info("Unlocking tag", "repository", repository, "tag", tag)
				if err := unlocker.UnlockTag(groupCtx, repository, tag); err != nil {
					r.logger.Warn("Failed to unlock tag; attempting deletion anyway", "repository", repository, "tag", tag, "err", err)
				}
				return nil
			})
		}
	}
	_ = group.Wait()
	return ctx.Err()
}
