package explore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/gdamore/tcell/v2"
)

type cancelFetchBackend struct {
	registry.Backend
	started chan struct{}
	once    atomic.Bool
}

func (b *cancelFetchBackend) GetManifest(ctx context.Context, repository, digest string) ([]byte, error) {
	if b.once.CompareAndSwap(false, true) {
		close(b.started)
		<-ctx.Done()
		return nil, &url.Error{Op: "Get", URL: "https://ghcr.io/v2/test/" + repository + "/manifests/" + digest, Err: ctx.Err()}
	}
	return b.Backend.GetManifest(ctx, repository, digest)
}

func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not start")
	}
}

func TestEscapeCancelsLiveOperationsAndAllowsRetry(t *testing.T) {
	for _, key := range []rune{'d', 'p', 'P', 'm', 'R'} {
		t.Run(string(key), func(t *testing.T) {
			b := registrytest.New()
			b.Add("one", registrytest.Image("one"), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
			backend := &cancelFetchBackend{Backend: b, started: make(chan struct{})}
			c := testClient(b)
			c.Rules = compileRules(t, `[{"repo":".+","untagged":[{"keep":false}]}]`)
			c.Connect = func(_ context.Context, logger *slog.Logger, _ bool) (*registry.Registry, error) {
				return registry.New(backend, logger, 1, nil)
			}
			a := newApp(sampleStats()[:1], Options{Client: c, LiveStats: true, Filter: "one"})
			a.repos.marked["one"] = true
			start := func() {
				press(t, a, tcell.KeyRune, key)
				if key == 'P' && a.panel == "repositories" {
					press(t, a, tcell.KeyEnter, 0)
				}
			}
			start()
			waitStarted(t, backend.started)
			press(t, a, tcell.KeyEsc, 0)
			if !a.job.stopping {
				t.Fatal("Esc did not stop the job")
			}
			r := finishJob(t, a)
			if !r.canceled || !errors.Is(r.err, context.Canceled) || r.failure() != nil {
				t.Fatal(r)
			}
			if a.panel != "" || !strings.Contains(a.status, "canceled") || a.plan != nil || len(a.targets) != 0 || a.stale {
				t.Fatalf("panel=%s status=%s stale=%t", a.panel, a.status, a.stale)
			}
			if !slices.Equal(a.repos.data, sampleStats()[:1]) || a.repos.query != "one" || !a.repos.marked["one"] || a.repository != "" {
				t.Fatal("cancellation changed the previous view")
			}
			start()
			if r := finishJob(t, a); r.err != nil || r.canceled {
				t.Fatalf("retry failed: %+v", r)
			}
			if key == 'd' || key == 'p' || key == 'P' {
				if a.panel != "plan" || len(a.targets) != 1 {
					t.Fatal("retry did not create a preview")
				}
			}
			if len(b.Deleted()) != 0 || len(b.DeletedRepositories()) != 0 {
				t.Fatal("cancellation or retry deleted data")
			}
		})
	}
}

func TestCancelDiscardsLateResults(t *testing.T) {
	// File reads may finish after Esc without observing the job's context;
	// live requests may also complete just before the cancellation reaches them.
	for _, kind := range []string{"preview", "statistics", "manifests", "reload", "rules"} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", kind, partial), func(t *testing.T) {
				c := testClient(registrytest.New())
				c.RuleSource = "original.json"
				a := newApp(sampleStats(), Options{Client: c, Filter: "one"})
				a.repos.marked["two"] = true
				a.repository = "one"
				a.manifests = newList([]pruner.RepositoryStats{{Name: "existing-image"}}, "name", "", false)
				a.manifests.marked["existing-image"] = true
				a.start(t.Context(), "Loading", func(ctx context.Context) result {
					<-ctx.Done()
					r := result{kind: kind, stats: []pruner.RepositoryStats{{Name: "replacement"}}, source: "new.json"}
					if partial {
						r.err = fmt.Errorf("request interrupted: %w", ctx.Err())
					}
					return r
				})
				press(t, a, tcell.KeyEsc, 0)
				finishJob(t, a)
				if a.panel != "" || !strings.Contains(a.status, "canceled") || a.plan != nil || a.stale {
					t.Fatal(a.panel, a.status)
				}
				if !slices.Equal(a.repos.data, sampleStats()) || a.repos.query != "one" || !a.repos.marked["two"] || c.RuleSource != "original.json" {
					t.Fatal("late result replaced previous state")
				}
				if a.repository != "one" || a.manifests.current() != "existing-image" || !a.manifests.marked["existing-image"] {
					t.Fatal("late result replaced the image view")
				}
			})
		}
	}
}

func TestCancelPreservesIndependentFailures(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("request failed"), errors.Join(context.Canceled, errors.New("cleanup failed"))} {
		t.Run(failure.Error(), func(t *testing.T) {
			a := newApp(sampleStats(), Options{})
			a.start(t.Context(), "Loading", func(ctx context.Context) result {
				<-ctx.Done()
				return result{kind: "statistics", stats: []pruner.RepositoryStats{{Name: "partial"}}, err: failure}
			})
			press(t, a, tcell.KeyEsc, 0)
			finishJob(t, a)
			if a.panel != "message" || a.message[0] != "ACTION FAILED" || !strings.Contains(strings.Join(a.message, " "), failure.Error()) {
				t.Fatal("separate failure hidden", a.message)
			}
			if !slices.Equal(a.repos.data, sampleStats()) {
				t.Fatal("canceled scan replaced the snapshot")
			}
		})
	}
}

type cancelDeleteBackend struct {
	*registrytest.Backend
	started chan struct{}
}

func (b *cancelDeleteBackend) DeleteRepository(ctx context.Context, name string) error {
	if name != "two" {
		return b.Backend.DeleteRepository(ctx, name)
	}
	close(b.started)
	<-ctx.Done()
	return fmt.Errorf("delete two: %w", ctx.Err())
}

func canceledDeletionClient(t *testing.T) (*registrytest.Backend, *cancelDeleteBackend, *Client) {
	t.Helper()
	b := registrytest.New()
	for _, name := range []string{"one", "two"} {
		b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour), Locked: true})
	}
	b.Fail = func(op, name string) error {
		if op == "RelockManifest" && name == "two" {
			return errors.New("restore failed")
		}
		return nil
	}
	backend := &cancelDeleteBackend{Backend: b, started: make(chan struct{})}
	c := testClient(b)
	c.IncludeLocked = true
	c.Connect = func(_ context.Context, logger *slog.Logger, _ bool) (*registry.Registry, error) {
		return registry.New(backend, logger, 1, nil)
	}
	return b, backend, c
}

func TestCanceledDeletionKeepsConfirmedCountsAndWarnings(t *testing.T) {
	b, backend, c := canceledDeletionClient(t)
	a := newApp(sampleStats()[:2], Options{Client: c, Logger: slog.New(&registrytest.Recorder{})})
	a.reviewable = true
	press(t, a, tcell.KeyRune, 'a')
	press(t, a, tcell.KeyRune, 'd')
	finishJob(t, a)
	press(t, a, tcell.KeyRune, 'c')
	typeText(t, a, a.confirmPhrase())
	press(t, a, tcell.KeyEnter, 0)
	waitStarted(t, backend.started)
	press(t, a, tcell.KeyEsc, 0)
	r := finishJob(t, a)
	if !r.canceled || r.failure() != nil || r.outcome.DeletedManifests != 1 || !slices.Equal(b.DeletedRepositories(), []string{"one"}) {
		t.Fatalf("canceled deletion lost outcome: %+v", r)
	}
	message := strings.Join(a.message, " ")
	for _, want := range []string{"DELETION CANCELED", "1 manifests, 1 repositories", "new preview", "stays unlocked", "two@", "restore failed"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %s", want, message)
		}
	}
	if strings.Contains(message, "context canceled") || a.plan != nil || len(a.targets) != 0 || !a.stale || len(a.repos.marked) != 0 {
		t.Fatal("canceled deletion retained plan or reported a cancellation error", message)
	}
	if len(a.completed) != 1 || len(a.completed[0].warnings) != 1 {
		t.Fatal("cleanup warnings missing from completion log")
	}
}

func TestCanceledDeletionLogsOutcomeAndCleanupWarningsOnExit(t *testing.T) {
	s := fakeScreen(t)
	_, backend, c := canceledDeletionClient(t)
	var logs strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), sampleStats()[:2], Options{Client: c, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	}()
	s.wait(t, "STATS EXPLORER")
	s.InjectKey(tcell.KeyRune, 'a', tcell.ModNone)
	s.InjectKey(tcell.KeyRune, 'd', tcell.ModNone)
	s.wait(t, "DELETION PREVIEW")
	s.InjectKey(tcell.KeyRune, 'c', tcell.ModNone)
	s.wait(t, "CONFIRM LIVE DELETION")
	for _, r := range "delete ghcr.io/test" {
		s.InjectKey(tcell.KeyRune, r, tcell.ModNone)
	}
	s.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	waitStarted(t, backend.started)
	s.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	if err := waitRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	text := logs.String()
	for _, want := range []string{"Explorer cleanup canceled", "confirmed.deleted_manifests=1", "confirmed.deleted_repos=1", "stays unlocked", "restore failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "context canceled") || strings.Contains(text, "error=") || s.finis.Load() != 1 {
		t.Fatal("cancellation logged as an error or terminal not restored", text)
	}
}

func TestEscapeWaitsForWorkAndReturnsToExplorer(t *testing.T) {
	s := fakeScreen(t)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c := &Client{Connect: func(ctx context.Context, _ *slog.Logger, _ bool) (*registry.Registry, error) {
		<-ctx.Done()
		<-release
		return nil, fmt.Errorf("manifest request: %w", ctx.Err())
	}}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), sampleStats(), Options{Client: c}) }()
	s.wait(t, "STATS EXPLORER")
	s.InjectKey(tcell.KeyRune, 'd', tcell.ModNone)
	s.wait(t, "Preparing deletion preview")
	s.InjectKey(tcell.KeyEsc, 0, tcell.ModNone)
	s.wait(t, "Stopping: waiting")
	if s.finis.Load() != 0 {
		t.Fatal("terminal closed while canceling")
	}
	close(release)
	s.wait(t, "Preview canceled; nothing was deleted")
	s.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	if err := waitRun(t, done); err != nil {
		t.Fatal("Esc left the explorer in a failed state", err)
	}
}

func TestQuitKeepsConcurrentFailures(t *testing.T) {
	for _, stop := range []string{"q", "ctrl-c", "context"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%t", stop, fail), func(t *testing.T) {
				s := fakeScreen(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				boom := errors.New("cleanup failed")
				c := &Client{Connect: func(ctx context.Context, _ *slog.Logger, _ bool) (*registry.Registry, error) {
					<-ctx.Done()
					if fail {
						return nil, errors.Join(ctx.Err(), boom)
					}
					return nil, fmt.Errorf("request: %w", ctx.Err())
				}}
				done := make(chan error, 1)
				go func() { done <- Run(ctx, nil, Options{Client: c, LiveStats: true}) }()
				s.wait(t, "Scanning registry statistics")
				switch stop {
				case "q":
					s.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
				case "ctrl-c":
					s.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
				case "context":
					cancel()
				}
				err := waitRun(t, done)
				if !errors.Is(err, context.Canceled) || errors.Is(err, boom) != fail || s.finis.Load() != 1 {
					t.Fatalf("error=%v restored=%d", err, s.finis.Load())
				}
			})
		}
	}
}
