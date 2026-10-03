package progress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/JohanLindvall/crprune/internal/cancellation"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

// Options identifies a long-running operation. Mode is auto, plain or tui.
type Options struct {
	Mode, Operation, Registry string
	DryRun                    bool
}

// ValidateMode reports whether mode is a valid --progress value: auto, plain
// or tui.
func ValidateMode(mode string) error {
	if mode != "auto" && mode != "plain" && mode != "tui" {
		return fmt.Errorf("invalid --progress %q: choose auto, plain or tui", mode)
	}
	return nil
}

// Seams for tests: the terminal screen, and whether stderr is a terminal.
var (
	newScreen        = tcell.NewScreen
	stderrIsTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
)

// Run uses a lightweight tcell screen for an interactive terminal, retaining
// plain logs for pipes and CI. The terminal is initialized before work starts
// and restored only after every worker has stopped. JSON stdout is untouched.
//
// A panic in work is held back until the terminal is restored and the
// summary printed, then raised again with the original stack. Panics in
// goroutines that work starts itself, such as the registry's download and
// deletion workers, are beyond its reach and still end the process at once.
func Run(ctx context.Context, opts Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
	if err := ValidateMode(opts.Mode); err != nil {
		return err
	}
	interactive := stderrIsTerminal() && os.Getenv("TERM") != "dumb"
	if opts.Mode == "plain" || opts.Mode == "auto" && !interactive {
		return work(ctx, logger)
	}
	if !interactive {
		return errors.New("--progress=tui needs an interactive terminal on stderr; use --progress=plain for redirected output")
	}
	screen, err := newScreen()
	if err == nil {
		if err = screen.Init(); err != nil {
			release(screen)
		}
	}
	if err != nil {
		if opts.Mode == "tui" {
			return fmt.Errorf("initialize progress display: %w", err)
		}
		logger.Warn("Terminal display unavailable; using plain progress", "err", err)
		return work(ctx, logger)
	}
	own(screen)
	t := NewTracker()
	func() {
		defer disown(screen)
		err = runScreen(ctx, screen, t, opts, logger, work)
	}()
	summarize(ctx, logger, opts, t.Snapshot(), err)
	if p, ok := errors.AsType[*panicError](err); ok {
		panic(p)
	}
	return err
}

// Both interfaces share ownership, including forced restoration on a signal.
var (
	own      = tui.Own
	disown   = tui.Disown
	onScreen = tui.OnScreen
	release  = tui.Release
)

// RestoreTerminal restores any active crprune terminal interface.
func RestoreTerminal() { tui.RestoreTerminal() }

// summarize leaves the outcome, and the warnings the display retained, in
// scrollback once the terminal is restored.
func summarize(ctx context.Context, logger *slog.Logger, opts Options, s Snapshot, err error) {
	status := "Completed"
	if cancellation.Only(err) {
		status = "Canceled"
	} else if err != nil {
		status = "Failed"
	}
	attrs := []any{"operation", opts.Operation, "repositories", s.Completed, "total", s.Repositories}
	if opts.Operation == "prune" {
		attrs = append(attrs, "dry_run", opts.DryRun, "kept", s.Kept, "selected", s.Selected, "deleted", s.Deleted)
	} else {
		attrs = append(attrs, "scanned", s.Kept, "unique", humanize.Bytes(s.Bytes))
	}
	attrs = append(attrs, "warnings", s.Warnings, "elapsed", time.Since(s.Started).Round(time.Second))
	logger.Info(status, attrs...)
	if s.DroppedWarnings > 0 {
		logger.Info("Earlier warnings were omitted from the bounded display; use --progress=plain to retain every warning", "omitted", s.DroppedWarnings)
	}
	// Preserve recent warnings in scrollback after leaving the alternate
	// screen, with the time each was logged.
	for _, entry := range s.WarningLogs {
		logger.Log(ctx, entry.Level, entry.Text, slog.Time("at", entry.Time))
	}
	if s.DroppedLogs > 0 {
		logger.Info("Earlier activity was omitted from the bounded display; use --progress=plain to retain a full log", "omitted", s.DroppedLogs)
	}
	// Only counts of a dry run's plan outlive the display, and the plan is
	// there to be reviewed.
	if opts.Operation == "prune" && opts.DryRun && s.Selected > 0 {
		logger.Info("Rerun with --progress=plain to list the manifests this dry run would delete", "selected", s.Selected)
	}
}

// panicError carries a panic in work out of its goroutine, which would
// otherwise end the process with the terminal still in raw mode.
type panicError struct {
	value any
	stack []byte
}

// Error returns the panic value followed by the stack it was raised on.
func (p *panicError) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

// Unwrap returns the panic value when it is an error.
func (p *panicError) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

func runScreen(ctx context.Context, screen tcell.Screen, tracker *Tracker, opts Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
	ctx, cancel := context.WithCancel(tracker.Context(ctx))
	defer cancel()
	log := slog.New(&logHandler{tracker: tracker, base: logger.Handler()})
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() {
			if value := recover(); value != nil {
				result <- &panicError{value: value, stack: debug.Stack()}
			}
		}()
		result <- work(ctx, log)
	}()
	// This also runs when drawing or syncing the screen panics. Workers
	// may need to restore registry locks after cancellation.
	defer func() { cancel(); <-finished }()

	events := make(chan tcell.Event, 8)
	quit := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		screen.ChannelEvents(events, quit)
	}()
	defer func() { close(quit); <-stopped }()
	tick := time.NewTicker(125 * time.Millisecond)
	defer tick.Stop()
	view := viewState{logs: true, color: os.Getenv("NO_COLOR") == ""}
	canceled := ctx.Done()
	for {
		onScreen(screen, func() { draw(screen, tracker.Snapshot(), opts, &view, time.Now()) })
		select {
		case err := <-result:
			return err
		case <-canceled:
			view.canceling = true
			canceled = nil
		case <-tick.C:
		case event, ok := <-events:
			if !ok {
				cancel()
				return errors.Join(errors.New("terminal event stream closed"), <-result)
			}
			switch event := event.(type) {
			case *tcell.EventError:
				cancel()
				return errors.Join(event, <-result)
			case *tcell.EventResize:
				onScreen(screen, screen.Sync)
			case *tcell.EventKey:
				if view.key(event) {
					cancel()
				}
			}
		}
	}
}

// viewState is the display state the keyboard controls.
type viewState struct {
	logs, warningsOnly, help, canceling, color bool

	// The activity panel follows the newest entry unless scrolled. Entries
	// are numbered counting those dropped before them, so that a scrolled
	// panel stays on the same entries while others arrive: end is the number
	// of the entry after the last one it shows. draw records the numbers of
	// the oldest entry (first) and of the one after the newest (newest), and
	// the rows the panel has, for the keys to scroll within.
	scrolled                 bool
	end, first, newest, rows int
}

// key applies a key press and reports whether it asks to cancel the run.
func (v *viewState) key(key *tcell.EventKey) bool {
	switch {
	case key.Key() == tcell.KeyCtrlC || key.Rune() == 'q':
		v.canceling = true
		return true
	case key.Rune() == 'l':
		v.logs = !v.logs
	case key.Rune() == 'w':
		v.warningsOnly = !v.warningsOnly
		v.scrolled = false
	case key.Rune() == '?':
		v.help = !v.help
	case key.Key() == tcell.KeyEsc:
		v.help = false
	case key.Key() == tcell.KeyUp || key.Rune() == 'k':
		v.scroll(-1)
	case key.Key() == tcell.KeyDown || key.Rune() == 'j':
		v.scroll(1)
	case key.Key() == tcell.KeyPgUp:
		v.scroll(-max(v.rows, 1))
	case key.Key() == tcell.KeyPgDn:
		v.scroll(max(v.rows, 1))
	case key.Key() == tcell.KeyHome:
		v.scrolled, v.end = true, v.first
		v.clamp()
	case key.Key() == tcell.KeyEnd:
		v.scrolled = false
	}
	return false
}

// scroll moves the activity panel by delta entries, towards newer ones when
// positive. Reaching the newest entry follows it again.
func (v *viewState) scroll(delta int) {
	if !v.scrolled {
		v.scrolled, v.end = true, v.newest
	}
	v.end += delta
	v.clamp()
}

// clamp keeps a scrolled panel within the entries it can show, so that no key
// press is spent scrolling beyond them.
func (v *viewState) clamp() {
	v.end = min(max(v.end, v.first+v.rows), v.newest)
	v.scrolled = v.scrolled && v.end < v.newest
}

// pin records the activity panel's geometry: rows visible, for entries
// numbered from first up to newest. It returns the number of the entry after
// the last one to show.
func (v *viewState) pin(first, newest, rows int) int {
	v.first, v.newest, v.rows = first, newest, rows
	v.clamp()
	if !v.scrolled {
		return newest
	}
	return v.end
}
