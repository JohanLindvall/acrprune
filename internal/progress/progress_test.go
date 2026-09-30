package progress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestTrackerConcurrentUpdatesAndBoundedLogs(t *testing.T) {
	tracker := NewTracker()
	ctx := tracker.Context(t.Context())
	Report(ctx, Event{Kind: Candidates, Total: 2})
	Report(ctx, Event{Kind: Repository, Name: "team/app"})
	logger := slog.New(&logHandler{tracker: tracker, base: slog.NewTextHandler(&strings.Builder{}, nil)})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				Report(ctx, Event{Kind: Listed, Count: 1})
				Report(ctx, Event{Kind: Fetched, Count: 1})
				Report(ctx, Event{Kind: Cached, Count: 1})
				logger.Warn("retry", "delay", time.Second)
				_ = tracker.Snapshot()
			}
		})
	}
	wg.Wait()
	Report(ctx, Event{Kind: Plan, Kept: 3, Count: 2, Bytes: 100})
	Report(ctx, Event{Kind: Deleted, Count: 1})
	Report(ctx, Event{Kind: Finished, Name: "denied"})
	Report(ctx, Event{Kind: Finished, Name: "skipped"})
	s := tracker.Snapshot()
	if s.Listed != 800 || s.Fetched != 800 || s.Cached != 800 || s.Warnings != 800 || s.DroppedLogs != 600 || len(s.Logs) != logLimit {
		t.Fatalf("updates lost: %+v", s)
	}
	if s.Completed != 2 || s.Denied != 1 || s.Skipped != 1 || s.Kept != 3 || s.Selected != 2 || s.Deleted != 1 || s.Bytes != 100 {
		t.Fatalf("incorrect totals: %+v", s)
	}
	s.Logs[0].Text = "modified copy"
	if tracker.Snapshot().Logs[0].Text == "modified copy" {
		t.Fatal("snapshot shares log storage")
	}
}

func TestLogHandlerPreservesGroupsAndSanitizesMessages(t *testing.T) {
	tracker := NewTracker()
	logger := slog.New(&logHandler{tracker: tracker, base: slog.NewTextHandler(&strings.Builder{}, nil)})
	logger.With("registry", "test").WithGroup("repo").With("name", "app").Info("message\x1b[2J\n", "count", 2, "manifest", slog.GroupValue(slog.String("tag", "v1")))
	logger.Debug("hidden")
	s := tracker.Snapshot()
	if len(s.Logs) != 1 {
		t.Fatalf("logs = %+v", s.Logs)
	}
	for _, want := range []string{"registry=test", "repo.name=app", "repo.count=2", "repo.manifest.tag=v1"} {
		if !strings.Contains(s.Logs[0].Text, want) {
			t.Errorf("missing %q in %q", want, s.Logs[0].Text)
		}
	}
	if strings.ContainsAny(s.Logs[0].Text, "\x1b\n") {
		t.Fatal("control characters reached the log view")
	}
}

func TestScreenCancellationWaitsForWorker(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(100, 30)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runScreen(t.Context(), screen, NewTracker(), Options{Operation: "prune", DryRun: true}, slog.New(slog.DiscardHandler),
			func(ctx context.Context, _ *slog.Logger) error {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			})
	}()
	<-started
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("q did not cancel work")
	}
	select {
	case err := <-result:
		t.Fatalf("returned before worker stopped: %v", err)
	default:
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestDrawResponsiveAndSafe(t *testing.T) {
	now := time.Now()
	s := Snapshot{Started: now.Add(-time.Minute), Repository: "team/app", Phase: "Inspecting manifests", Repositories: 10, Completed: 3,
		Listed: 100, Fetched: 70, Kept: 20, Selected: 50, Bytes: 123456789, Warnings: 1,
		Logs: []Entry{{Time: now, Level: slog.LevelWarn, Text: "retry\x1b[2J\n界界界"}}}
	for _, size := range [][2]int{{120, 32}, {80, 24}, {44, 10}, {1, 1}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(size[0], size[1])
			draw(screen, s, Options{Operation: "prune", Registry: "ghcr.io/acme", DryRun: true}, viewState{logs: true}, now)
			text := screenText(screen)
			if strings.ContainsAny(text, "\x1b") {
				t.Fatal("control sequence rendered")
			}
			if size[0] >= 80 {
				for _, want := range []string{"DRY RUN", "team/app", "3 / 10", "SELECTED  50", "DELETED  0"} {
					if !strings.Contains(text, want) {
						t.Errorf("missing %q:\n%s", want, text)
					}
				}
			}
		})
	}
}

func screenText(screen tcell.SimulationScreen) string {
	cells, w, _ := screen.GetContents()
	var text strings.Builder
	for i, cell := range cells {
		if len(cell.Runes) == 0 {
			text.WriteByte(' ')
		} else {
			text.WriteString(string(cell.Runes))
		}
		if (i+1)%w == 0 {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func TestKeysAndPlainMode(t *testing.T) {
	v := viewState{}
	for _, ch := range "lw?k" {
		v.key(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
	}
	if !v.logs || !v.warningsOnly || !v.help || v.offset != 1 {
		t.Fatalf("state = %+v", v)
	}
	v.key(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
	if v.offset != 0 {
		t.Fatal("End should follow the newest entry")
	}
	boom := errors.New("operation failed")
	err := Run(t.Context(), Options{Mode: "plain"}, slog.New(slog.DiscardHandler), func(context.Context, *slog.Logger) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if ValidateMode("bogus") == nil {
		t.Fatal("invalid mode accepted")
	}
}
