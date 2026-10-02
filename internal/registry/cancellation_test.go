package registry_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
)

type canceledUnlockBackend struct {
	*registrytest.Backend
	cancel context.CancelFunc
}

func (b canceledUnlockBackend) UnlockManifest(ctx context.Context, m *registry.Manifest) (registry.Relock, error) {
	restore, err := b.Backend.UnlockManifest(ctx, m)
	if err != nil {
		return restore, err
	}
	b.cancel()
	return restore, fmt.Errorf("unlock response: %w", ctx.Err())
}

func TestCanceledUnlockRestoresLocksWithoutCancellationWarning(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, restoreFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("whole=%t/restoreFails=%t", whole, restoreFails), func(t *testing.T) {
				fake := registrytest.New()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reg, logs := newLoggedRegistry(t, canceledUnlockBackend{fake, cancel}, nil)
				locked, _ := lockedRepository(t, fake, reg)
				fake.Fail = func(op, _ string) error {
					if op == "RelockManifest" && restoreFails {
						return errors.New("restore failed")
					}
					return nil
				}
				var err error
				if whole {
					err = reg.DeleteRepository(ctx, "app", []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true})
				} else {
					_, err = reg.DeleteManifests(ctx, []*registry.Manifest{locked}, registry.DeleteOptions{Unlock: true})
				}
				if !errors.Is(err, context.Canceled) || fake.Calls("RelockManifest") != 1 || fake.Calls("UnlockTag") != 0 || fake.Calls("DeleteManifest") != 0 || fake.Calls("DeleteRepository") != 0 {
					t.Fatalf("cancellation did not stop deletion and restore locks: %v", err)
				}
				warnings := logs.Messages(slog.LevelWarn)
				if !restoreFails && len(warnings) != 0 {
					t.Fatal("cancellation logged as a warning", warnings)
				}
				if restoreFails && (len(warnings) != 1 || !strings.Contains(warnings[0], "restore failed") || !strings.Contains(warnings[0], "may stay unlocked")) {
					t.Fatal("real restoration failure was hidden", warnings)
				}
				if !restoreFails && !slices.Equal(fake.Relocked(), []string{"app@" + locked.Digest}) {
					t.Fatal("removed lock not restored", fake.Relocked())
				}
			})
		}
	}
}

func TestCanceledDeletionPreservesJoinedFailure(t *testing.T) {
	fake := registrytest.New()
	digest := fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	m := fetch(t, newRegistry(t, fake, nil), "app", registry.FetchOptions{})[digest]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	refused := registrytest.Forbidden("app")
	backend := interruptingBackend{Backend: fake, cancel: cancel, err: errors.Join(context.Canceled, refused)}
	_, err := newRegistry(t, backend, nil).DeleteManifests(ctx, []*registry.Manifest{m}, registry.DeleteOptions{})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, refused) {
		t.Fatalf("lost refusal accompanying cancellation: %v", err)
	}
}
