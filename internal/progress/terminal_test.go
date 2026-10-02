package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// fakeTerminal makes Run see an interactive terminal on stderr, whose screen
// is what newScreen returns.
func fakeTerminal(t *testing.T, screen func() (tcell.Screen, error)) {
	t.Helper()
	oldScreen, oldTerminal := newScreen, stderrIsTerminal
	t.Cleanup(func() { newScreen, stderrIsTerminal = oldScreen, oldTerminal })
	newScreen = screen
	stderrIsTerminal = func() bool { return true }
	t.Setenv("TERM", "xterm-256color")
}

// brokenScreen fails to initialize as tcell does without a controlling
// terminal, and then panics in Fini as tcell does after such a failure.
type brokenScreen struct {
	tcell.Screen
	finis int
}

func (s *brokenScreen) Init() error {
	return errors.New("open /dev/tty: no such device or address")
}

func (s *brokenScreen) Fini() {
	s.finis++
	panic("close of nil channel")
}

// watchedScreen is a simulated terminal that publishes its text after every
// Show, for tests to wait on while runScreen draws on its own goroutine.
type watchedScreen struct {
	tcell.SimulationScreen
	// hangUp, once closed, ends the event stream as a terminal that went
	// away does.
	hangUp chan struct{}
	// onFini, when set, runs as the terminal is restored.
	onFini func()

	mu    sync.Mutex
	text  string
	shown chan struct{}
}

func newWatchedScreen(w, h int) *watchedScreen {
	screen := &watchedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), hangUp: make(chan struct{}), shown: make(chan struct{}, 1)}
	if err := screen.Init(); err != nil {
		panic(err)
	}
	screen.SetSize(w, h)
	return screen
}

func (s *watchedScreen) Show() {
	s.SimulationScreen.Show()
	text := screenText(s.SimulationScreen)
	s.mu.Lock()
	s.text = text
	s.mu.Unlock()
	select {
	case s.shown <- struct{}{}:
	default:
	}
}

func (s *watchedScreen) Fini() {
	if s.onFini != nil {
		s.onFini()
	}
	s.SimulationScreen.Fini()
}

func (s *watchedScreen) ChannelEvents(ch chan<- tcell.Event, quit <-chan struct{}) {
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		select {
		case <-quit:
		case <-s.hangUp:
		}
	}()
	s.SimulationScreen.ChannelEvents(ch, stop)
}

// waitFor waits until the screen shows what ok looks for.
func (s *watchedScreen) waitFor(t *testing.T, what string, ok func(text string) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		text := s.text
		s.mu.Unlock()
		if ok(text) {
			return
		}
		select {
		case <-s.shown:
		case <-deadline:
			t.Fatalf("the screen never showed %s:\n%s", what, text)
		}
	}
}

func (s *watchedScreen) waitForText(t *testing.T, want string) {
	t.Helper()
	s.waitFor(t, fmt.Sprintf("%q", want), func(text string) bool { return strings.Contains(text, want) })
}

func (s *watchedScreen) waitForNo(t *testing.T, unwanted string) {
	t.Helper()
	s.waitFor(t, fmt.Sprintf("no %q", unwanted), func(text string) bool { return !strings.Contains(text, unwanted) })
}

// TestRunFallsBackWithoutTerminal: a terminal that cannot be initialized
// falls back to plain logs in auto mode and is an error in tui mode; tcell
// panicking as it is released must not crash the run.
func TestRunFallsBackWithoutTerminal(t *testing.T) {
	var broken *brokenScreen
	for _, tt := range []struct {
		name   string
		screen func() (tcell.Screen, error)
	}{
		{"init fails", func() (tcell.Screen, error) { broken = &brokenScreen{}; return broken, nil }},
		{"no screen", func() (tcell.Screen, error) { return nil, errors.New("terminal entry not found") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeTerminal(t, tt.screen)
			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			ran := false
			work := func(_ context.Context, l *slog.Logger) error {
				ran = l == logger
				return nil
			}
			if err := Run(t.Context(), Options{Mode: "auto"}, logger, work); err != nil || !ran {
				t.Errorf("auto: error = %v, work ran with the plain logger = %v", err, ran)
			}
			if !strings.Contains(logs.String(), "Terminal display unavailable; using plain progress") {
				t.Errorf("auto mode did not report the fallback:\n%s", logs.String())
			}
			ran = false
			if err := Run(t.Context(), Options{Mode: "tui"}, logger, work); err == nil || !strings.Contains(err.Error(), "initialize progress display") || ran {
				t.Errorf("tui: error = %v, work ran = %v", err, ran)
			}
		})
	}
	if broken == nil || broken.finis != 1 {
		t.Error("a screen that failed to initialize should be released")
	}
}

func TestRunChoosesPlainLogs(t *testing.T) {
	fakeTerminal(t, func() (tcell.Screen, error) {
		t.Fatal("no screen should be created")
		return nil, nil
	})
	logger := slog.New(slog.DiscardHandler)
	plain := func(_ context.Context, l *slog.Logger) error {
		if l != logger {
			return errors.New("work did not get the plain logger")
		}
		return nil
	}
	t.Setenv("TERM", "dumb")
	if err := Run(t.Context(), Options{Mode: "auto"}, logger, plain); err != nil {
		t.Errorf("TERM=dumb: %v", err)
	}
	t.Setenv("TERM", "xterm")
	stderrIsTerminal = func() bool { return false }
	if err := Run(t.Context(), Options{Mode: "auto"}, logger, plain); err != nil {
		t.Errorf("redirected stderr: %v", err)
	}
	if err := Run(t.Context(), Options{Mode: "tui"}, logger, plain); err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
		t.Errorf("tui without a terminal: error = %v", err)
	}
}

// TestRunSummarizes: once the terminal is restored, Run leaves a summary
// fitting the operation, and the warnings of the run, in scrollback.
func TestRunSummarizes(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name           string
		opts           Options
		work           func(context.Context, *slog.Logger) error
		want, unwanted []string
		wantedWarnings int
		wantErr        error
	}{
		{
			name: "prune",
			opts: Options{Mode: "tui", Operation: "prune", DryRun: true},
			work: func(ctx context.Context, logger *slog.Logger) error {
				Report(ctx, Event{Kind: Candidates, Total: 2})
				Report(ctx, Event{Kind: Plan, Kept: 3, Count: 2, Bytes: 100})
				Report(ctx, Event{Kind: Finished})
				logger.Warn("Keeping the last tagged manifest", "repository", "app")
				for i := range logLimit + 50 {
					logger.Info("Dry-run: deleting manifest", "n", i)
				}
				return nil
			},
			want: []string{
				"level=INFO msg=Completed operation=prune repositories=1 total=2 dry_run=true kept=3 selected=2 deleted=0 warnings=1",
				`level=WARN msg="Keeping the last tagged manifest  repository=app" at=`,
				"Earlier activity was omitted from the bounded display; use --progress=plain to retain a full log\" omitted=51",
				"msg=\"Rerun with --progress=plain to list the manifests this dry run would delete\" selected=2\n",
			},
			unwanted:       []string{"scanned=", "Earlier warnings"},
			wantedWarnings: 1,
		},
		{
			name: "dry run selecting nothing",
			opts: Options{Mode: "tui", Operation: "prune", DryRun: true},
			work: func(ctx context.Context, logger *slog.Logger) error {
				Report(ctx, Event{Kind: Plan, Kept: 3})
				return nil
			},
			want:     []string{"msg=Completed operation=prune repositories=0 total=0 dry_run=true kept=3 selected=0"},
			unwanted: []string{"Rerun with"},
		},
		{
			name: "live run",
			opts: Options{Mode: "tui", Operation: "prune"},
			work: func(ctx context.Context, logger *slog.Logger) error {
				Report(ctx, Event{Kind: Plan, Count: 2})
				Report(ctx, Event{Kind: Deleted, Count: 2})
				return nil
			},
			want:     []string{"dry_run=false kept=0 selected=2 deleted=2"},
			unwanted: []string{"Rerun with"},
		},
		{
			name: "statistics",
			opts: Options{Mode: "tui", Operation: "statistics"},
			work: func(ctx context.Context, logger *slog.Logger) error {
				Report(ctx, Event{Kind: Plan, Kept: 40, Bytes: 2_000_000})
				for i := range warningLimit + 20 {
					logger.Warn("Manifest missing", "n", i)
				}
				return nil
			},
			want: []string{
				"msg=Completed operation=statistics repositories=0 total=0 scanned=40 unique=\"2.0 MB\" warnings=120",
				"Earlier warnings were omitted from the bounded display; use --progress=plain to retain every warning\" omitted=20",
				`msg="Manifest missing  n=119"`,
			},
			unwanted:       []string{"selected=", "dry_run=", "n=19\"", "Earlier activity"},
			wantedWarnings: warningLimit,
		},
		{
			name:     "failed",
			opts:     Options{Mode: "tui", Operation: "prune"},
			work:     func(context.Context, *slog.Logger) error { return boom },
			want:     []string{"msg=Failed operation=prune"},
			wantErr:  boom,
			unwanted: []string{"omitted"},
		},
		{
			name:    "canceled",
			opts:    Options{Mode: "auto", Operation: "prune"},
			work:    func(context.Context, *slog.Logger) error { return fmt.Errorf("stopped: %w", context.Canceled) },
			want:    []string{"msg=Canceled operation=prune"},
			wantErr: context.Canceled,
		},
		{
			name:     "cancellation and failure",
			opts:     Options{Mode: "tui", Operation: "prune"},
			work:     func(context.Context, *slog.Logger) error { return errors.Join(context.Canceled, boom) },
			want:     []string{"msg=Failed operation=prune"},
			wantErr:  boom,
			unwanted: []string{"msg=Canceled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs strings.Builder
			screen := newWatchedScreen(100, 30)
			screen.onFini = func() { logs.WriteString("terminal restored\n") }
			fakeTerminal(t, func() (tcell.Screen, error) { return screen, nil })
			err := Run(t.Context(), tt.opts, slog.New(slog.NewTextHandler(&logs, nil)), tt.work)
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			out := logs.String()
			if !strings.HasPrefix(out, "terminal restored\n") {
				t.Errorf("the summary should follow restoring the terminal:\n%s", out)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in\n%s", want, out)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(out, unwanted) {
					t.Errorf("unexpected %q in\n%s", unwanted, out)
				}
			}
			if got := strings.Count(out, "level=WARN"); got != tt.wantedWarnings {
				t.Errorf("replayed %d warnings, want %d", got, tt.wantedWarnings)
			}
		})
	}
}

// tally is never made, so writing to it panics.
var tally map[string]int

// explode is work with a bug that panics.
func explode(context.Context, *slog.Logger) error {
	tally["boom"]++
	return nil
}

// TestRunRestoresTerminalBeforePanicking: a panic in work surfaces from Run,
// with the stack it was raised on, after the terminal is restored.
func TestRunRestoresTerminalBeforePanicking(t *testing.T) {
	var logs strings.Builder
	screen := newWatchedScreen(100, 30)
	restored := false
	screen.onFini = func() { restored = true }
	fakeTerminal(t, func() (tcell.Screen, error) { return screen, nil })
	defer func() {
		value := recover()
		err, ok := value.(error)
		if !ok {
			t.Fatalf("panic value = %#v, want an error", value)
		}
		if !restored {
			t.Error("the terminal was not restored before the panic")
		}
		if msg := err.Error(); !strings.Contains(msg, "assignment to entry in nil map") || !strings.Contains(msg, "progress.explode(") {
			t.Errorf("the panic lost its value or original stack:\n%s", msg)
		}
		if _, ok := errors.AsType[runtime.Error](err); !ok {
			t.Error("the runtime error should remain reachable")
		}
		if !strings.Contains(logs.String(), "msg=Failed") {
			t.Errorf("no summary before the panic:\n%s", logs.String())
		}
	}()
	_ = Run(t.Context(), Options{Mode: "tui", Operation: "prune"}, slog.New(slog.NewTextHandler(&logs, nil)), explode)
	t.Fatal("Run returned instead of panicking")
}

// TestRestoreTerminal: a process exiting on a second interrupt left the
// terminal raw, on the alternate screen. RestoreTerminal restores it while
// Run runs, from any goroutine and only once, and the display draws nothing
// more and stops.
func TestRestoreTerminal(t *testing.T) {
	RestoreTerminal() // no display owns the terminal: nothing to do
	screen := newWatchedScreen(100, 30)
	var finis atomic.Int32
	screen.onFini = func() { finis.Add(1) }
	fakeTerminal(t, func() (tcell.Screen, error) { return screen, nil })
	result := make(chan error, 1)
	go func() {
		result <- Run(t.Context(), Options{Mode: "tui", Operation: "prune"}, slog.New(slog.NewTextHandler(io.Discard, nil)), logAndWait(1))
	}()
	screen.waitForText(t, "entry 00")

	var restorers sync.WaitGroup
	for range 4 {
		restorers.Go(RestoreTerminal)
	}
	restorers.Wait()
	if n := finis.Load(); n != 1 {
		t.Errorf("terminal restored %d times, want once", n)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "terminal event stream closed") {
			t.Errorf("error = %v, want the display stopped with its terminal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the display did not stop")
	}
	RestoreTerminal()
	if n := finis.Load(); n != 1 {
		t.Errorf("terminal restored %d times in all, want once", n)
	}
}

// runWatched runs work on screen in the background, as Run does once the
// screen owns the terminal, returning runScreen's result. The test's end
// cancels it.
func runWatched(t *testing.T, screen *watchedScreen, work func(context.Context, *slog.Logger) error) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	done := make(chan struct{})
	own(screen)
	go func() {
		defer close(done)
		result <- runScreen(t.Context(), screen, NewTracker(), Options{Operation: "prune"}, slog.New(slog.NewTextHandler(io.Discard, nil)), work)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runScreen did not return")
		}
		disown(screen)
	})
	return result
}

// logAndWait logs n entries and waits to be canceled.
func logAndWait(n int) func(context.Context, *slog.Logger) error {
	return func(ctx context.Context, logger *slog.Logger) error {
		for i := range n {
			logger.Info(fmt.Sprintf("entry %02d", i))
		}
		logger.Warn("Retrying request")
		<-ctx.Done()
		return ctx.Err()
	}
}

// TestRunScreenKeysAndResize drives the display through its event loop.
func TestRunScreenKeysAndResize(t *testing.T) {
	screen := newWatchedScreen(100, 30)
	result := runWatched(t, screen, logAndWait(30))
	screen.waitForText(t, "entry 29")

	screen.InjectKey(tcell.KeyHome, 0, tcell.ModNone)
	screen.waitFor(t, "the oldest entries, scrolled", func(text string) bool {
		return strings.Contains(text, "entry 00") && !strings.Contains(text, "entry 29") && strings.Contains(text, "scrolled")
	})
	screen.InjectKey(tcell.KeyRune, 'w', tcell.ModNone)
	screen.waitForText(t, "ACTIVITY · warnings only")
	screen.waitForNo(t, "entry 00")
	screen.InjectKey(tcell.KeyRune, '?', tcell.ModNone)
	screen.waitForText(t, "KEYBOARD")
	screen.InjectKey(tcell.KeyEsc, 0, tcell.ModNone)
	screen.waitForNo(t, "KEYBOARD")
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	screen.waitForNo(t, "ACTIVITY")

	screen.SetSize(40, 10)
	if err := screen.PostEvent(tcell.NewEventResize(40, 10)); err != nil {
		t.Fatal(err)
	}
	screen.waitForText(t, "enlarge for details")
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
}

// TestRunScreenStopsWithTerminal: a failing terminal cancels the work and
// ends the display with the failure.
func TestRunScreenStopsWithTerminal(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail func(*watchedScreen)
		want string
	}{
		{"event error", func(s *watchedScreen) { _ = s.PostEvent(tcell.NewEventError(errors.New("tty lost"))) }, "tty lost"},
		{"event stream closed", func(s *watchedScreen) { close(s.hangUp) }, "terminal event stream closed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			screen := newWatchedScreen(100, 30)
			result := runWatched(t, screen, logAndWait(1))
			screen.waitForText(t, "entry 00")
			tt.fail(screen)
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, context.Canceled) {
					t.Errorf("error = %v, want %q and the canceled work's error", err, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the display did not stop")
			}
		})
	}
}
