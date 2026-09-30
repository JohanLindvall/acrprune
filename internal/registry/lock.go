package registry

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/JohanLindvall/acrprune/internal/imageref"
)

// relockTimeout bounds how long restoring the locks of a failed deletion may
// take. The restore runs even after cancellation, which is what fails the
// deletion when the user interrupts the run.
const relockTimeout = 30 * time.Second

// LoadTagLocks records in the LockedTags of each manifest which of its tags are
// locked, on registries with locks. It lists the repository's tags once, and
// only when one of the manifests is tagged; otherwise, and when the repository
// no longer exists, it leaves LockedTags as they are.
func (r *Registry) LoadTagLocks(ctx context.Context, repository string, manifests []*Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlocker, ok := r.backend.(Unlocker)
	if !ok || !slices.ContainsFunc(manifests, func(m *Manifest) bool { return len(m.Tags) > 0 }) {
		return nil
	}
	if !imageref.ValidRepository(repository) {
		return fmt.Errorf("invalid repository name %q", repository)
	}
	locked, err := unlocker.LockedTags(ctx, repository)
	if IsNotFound(err) {
		r.logger.Debug("Repository gone; no tag locks to load", "repository", repository)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to list tags for %s: %w", repository, err)
	}
	for _, m := range manifests {
		m.LockedTags = lockedTagsOf(m.Tags, locked)
	}
	return nil
}

// lockedTagsOf returns the tags that are locked, empty but not nil when none
// is.
func lockedTagsOf(tags []string, locked map[string]bool) []string {
	result := []string{}
	for _, tag := range tags {
		if locked[tag] {
			result = append(result, tag)
		}
	}
	return result
}

// relock restores one lock removed by unlock.
type relock struct {
	what    string // manifest reference or repository:tag, for logging
	restore Relock
	// uncertain marks a lock whose removal failed, and so may or may not
	// have happened.
	uncertain bool
}

// unlock re-enables delete and write on m, when it is locked, and on its
// locked tags, returning what restores them. Failures are logged and
// tolerated: the deletion that follows reports anything that genuinely could
// not be unlocked, and a lock whose state could not be read is left alone. A
// failed unlock that returned a Relock may have removed the lock all the same
// — an interrupted request can take effect — so it is restored too.
func (r *Registry) unlock(ctx context.Context, unlocker Unlocker, m *Manifest) []relock {
	var relocks []relock
	add := func(what string, restore Relock, err error) {
		if err != nil {
			r.logger.Warn("Failed to unlock; attempting deletion anyway", "unlocking", what, "err", err)
		}
		if restore != nil {
			relocks = append(relocks, relock{what: what, restore: restore, uncertain: err != nil})
		}
	}
	if m.Locked {
		r.logger.Info("Unlocking manifest", "manifest", m)
		restore, err := unlocker.UnlockManifest(ctx, m)
		add(m.Ref(), restore, err)
	}
	for _, tag := range m.LockedTags {
		r.logger.Info("Unlocking tag", "repository", m.Repository, "tag", tag)
		restore, err := unlocker.UnlockTag(ctx, m.Repository, tag)
		add(m.Repository+":"+tag, restore, err)
	}
	return relocks
}

// unlockAll unlocks the manifests in parallel, as unlock does one.
func (r *Registry) unlockAll(ctx context.Context, unlocker Unlocker, manifests []*Manifest) []relock {
	var mu sync.Mutex
	var relocks []relock
	group := r.pool()
	for _, m := range manifests {
		if !m.IsLocked() {
			continue
		}
		group.Go(func() error {
			restored := r.unlock(ctx, unlocker, m)
			mu.Lock()
			defer mu.Unlock()
			relocks = append(relocks, restored...)
			return nil
		})
	}
	_ = group.Wait()
	return relocks
}

// relock restores the locks unlock removed, after the deletion they made way
// for failed, logging each one that stays unlocked. It runs even when ctx is
// canceled — that is what fails the deletion when the run is interrupted.
func (r *Registry) relock(ctx context.Context, relocks []relock) {
	if len(relocks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relockTimeout)
	defer cancel()
	group := r.pool()
	for _, l := range relocks {
		group.Go(func() error {
			switch err := l.restore(ctx); {
			case err != nil && l.uncertain:
				r.logger.Warn("Failed to restore lock after failed unlock and deletion; it may stay unlocked", "unlocked", l.what, "err", err)
			case err != nil:
				r.logger.Warn("Failed to restore lock after failed deletion; it stays unlocked", "unlocked", l.what, "err", err)
			default:
				r.logger.Info("Restored lock after failed deletion", "locked", l.what)
			}
			return nil
		})
	}
	_ = group.Wait()
}
