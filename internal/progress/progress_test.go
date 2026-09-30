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
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

func TestTrackerConcurrentUpdatesAndBoundedLogs(t *testing.T) {
	tracker := NewTracker()
	ctx := tracker.Context(t.Context())
	Report(ctx, Event{Kind: Candidates, Total: 3})
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
	Report(ctx, Event{Kind: Finished, Outcome: Denied})
	Report(ctx, Event{Kind: Finished, Outcome: Skipped})
	Report(ctx, Event{Kind: Finished})
	s := tracker.Snapshot()
	if s.Listed != 800 || s.Fetched != 800 || s.Cached != 800 || s.Warnings != 800 || s.DroppedLogs != 600 || len(s.Logs) != logLimit {
		t.Fatalf("updates lost: %+v", s)
	}
	if s.DroppedWarnings != 800-warningLimit || len(s.WarningLogs) != warningLimit {
		t.Fatalf("warnings ring holds %d, dropped %d", len(s.WarningLogs), s.DroppedWarnings)
	}
	if s.Completed != 3 || s.Denied != 1 || s.Skipped != 1 || s.Kept != 3 || s.Selected != 2 || s.Deleted != 1 || s.Bytes != 100 {
		t.Fatalf("incorrect totals: %+v", s)
	}
	s.Logs[0].Text = "modified copy"
	s.WarningLogs[0].Text = "modified copy"
	if again := tracker.Snapshot(); again.Logs[0].Text == "modified copy" || again.WarningLogs[0].Text == "modified copy" {
		t.Fatal("snapshot shares log storage")
	}
}

// TestTrackerRepositoryCounts: the manifest counters shown next to each other
// all describe the current repository.
func TestTrackerRepositoryCounts(t *testing.T) {
	tracker := NewTracker()
	ctx := tracker.Context(t.Context())
	Report(ctx, Event{Kind: Repository, Name: "first"})
	Report(ctx, Event{Kind: Listed, Count: 4})
	Report(ctx, Event{Kind: Fetched, Count: 4})
	Report(ctx, Event{Kind: Cached, Count: 3})
	Report(ctx, Event{Kind: Plan, Kept: 1, Count: 3})
	Report(ctx, Event{Kind: Repository, Name: "second"})
	Report(ctx, Event{Kind: Phase, Name: "Deleting manifests"})
	s := tracker.Snapshot()
	if s.Listed != 0 || s.Fetched != 0 || s.Cached != 0 {
		t.Errorf("per-repository counts carried over: listed %d, fetched %d, cached %d", s.Listed, s.Fetched, s.Cached)
	}
	if s.Repository != "second" || s.Phase != "Deleting manifests" || s.Kept != 1 || s.Selected != 3 {
		t.Errorf("snapshot = %+v", s)
	}
}

// TestWarningsOutliveActivity: a busy run's info lines push warnings out of
// the activity log, but not out of the warnings kept for the warnings-only
// view and the replay after the run.
func TestWarningsOutliveActivity(t *testing.T) {
	tracker := NewTracker()
	logger := slog.New(&logHandler{tracker: tracker, base: slog.NewTextHandler(&strings.Builder{}, nil)})
	logger.Warn("Skipping repository with unavailable manifest documents", "repository", "app")
	for i := range logLimit + 50 {
		logger.Info("Dry-run: deleting manifest", "n", i)
	}
	s := tracker.Snapshot()
	for _, entry := range s.Logs {
		if entry.Level >= slog.LevelWarn {
			t.Fatalf("the warning should have left the activity log: %+v", entry)
		}
	}
	if s.Warnings != 1 || len(s.WarningLogs) != 1 || s.DroppedWarnings != 0 || !strings.HasPrefix(s.WarningLogs[0].Text, "Skipping repository") {
		t.Errorf("warnings = %d, retained %+v", s.Warnings, s.WarningLogs)
	}
}

func TestLogHandlerPreservesGroupsAndSanitizesMessages(t *testing.T) {
	tracker := NewTracker()
	logger := slog.New(&logHandler{tracker: tracker, base: slog.NewTextHandler(&strings.Builder{}, nil)})
	logger.With("registry", "test").WithGroup("repo").With("name", "app").Info("message\x1b[2J\n", "count", 2, "manifest", slog.GroupValue(slog.String("tag", "v1")))
	logger.Debug("hidden")
	logger.Info("bare", slog.Attr{})
	s := tracker.Snapshot()
	if len(s.Logs) != 2 {
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
	if s.Logs[1].Text != "bare" {
		t.Errorf("an empty attribute should be ignored: %q", s.Logs[1].Text)
	}
}

// TestLogHandlerBoundsText: an entry keeps a bounded amount of text, cut
// between characters.
func TestLogHandlerBoundsText(t *testing.T) {
	tracker := NewTracker()
	logger := slog.New(&logHandler{tracker: tracker, base: slog.NewTextHandler(&strings.Builder{}, nil)})
	logger.Warn("Orphaned manifest", "document", strings.Repeat("界", maxEntryText))
	text := tracker.Snapshot().WarningLogs[0].Text
	if len(text) > maxEntryText+len("…") || !strings.HasSuffix(text, "…") || !utf8.ValidString(text) {
		t.Errorf("entry text of %d bytes, ending %q", len(text), text[max(0, len(text)-8):])
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate changed short text: %q", got)
	}
}

func TestKindString(t *testing.T) {
	if Finished.String() != "Finished" || Candidates.String() != "Candidates" || Kind(99).String() != "Kind(99)" {
		t.Errorf("kind names: %v %v %v", Finished, Candidates, Kind(99))
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
	for _, size := range [][2]int{{120, 32}, {80, 24}, {44, 10}, {1, 1}, {0, 0}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(size[0], size[1])
			draw(screen, s, Options{Operation: "prune", Registry: "ghcr.io/acme", DryRun: true}, &viewState{logs: true}, now)
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

func TestPlainModeAndValidation(t *testing.T) {
	boom := errors.New("operation failed")
	err := Run(t.Context(), Options{Mode: "plain"}, slog.New(slog.DiscardHandler), func(context.Context, *slog.Logger) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if ValidateMode("bogus") == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := Run(t.Context(), Options{Mode: "bogus"}, slog.New(slog.DiscardHandler), nil); err == nil {
		t.Fatal("Run accepted an invalid mode")
	}
}
