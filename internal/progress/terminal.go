package progress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

// Options identifies a long-running operation. Mode is auto, plain or tui.
type Options struct {
	Mode, Operation, Registry string
	DryRun                    bool
}

func ValidateMode(mode string) error {
	if mode != "auto" && mode != "plain" && mode != "tui" {
		return fmt.Errorf("invalid --progress %q: choose auto, plain or tui", mode)
	}
	return nil
}

// Run uses a lightweight tcell screen for an interactive terminal, retaining
// plain logs for pipes and CI. The terminal is initialized before work starts
// and restored only after every worker has stopped. JSON stdout is untouched.
func Run(ctx context.Context, opts Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
	if err := ValidateMode(opts.Mode); err != nil {
		return err
	}
	interactive := term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("TERM") != "dumb"
	if opts.Mode == "plain" || opts.Mode == "auto" && !interactive {
		return work(ctx, logger)
	}
	if !interactive {
		return errors.New("--progress=tui needs an interactive terminal on stderr; use --progress=plain for redirected output")
	}
	screen, err := tcell.NewScreen()
	if err == nil {
		err = screen.Init()
	}
	if err != nil {
		if screen != nil {
			screen.Fini()
		}
		if opts.Mode == "tui" {
			return fmt.Errorf("initialize progress display: %w", err)
		}
		logger.Warn("Terminal display unavailable; using plain progress", "err", err)
		return work(ctx, logger)
	}
	t := NewTracker()
	func() {
		defer screen.Fini()
		err = runScreen(ctx, screen, t, opts, logger, work)
	}()
	s := t.Snapshot()
	status := "Completed"
	if errors.Is(err, context.Canceled) {
		status = "Canceled"
	} else if err != nil {
		status = "Failed"
	}
	logger.Info(status, "operation", opts.Operation, "dry_run", opts.DryRun,
		"repositories", s.Completed, "total", s.Repositories, "kept", s.Kept,
		"selected", s.Selected, "deleted", s.Deleted, "warnings", s.Warnings,
		"elapsed", time.Since(s.Started).Round(time.Second))
	// Preserve recent warnings in scrollback after leaving the alternate screen.
	for _, entry := range s.Logs {
		if entry.Level >= slog.LevelWarn {
			logger.Log(ctx, entry.Level, entry.Text)
		}
	}
	if s.DroppedLogs > 0 {
		logger.Info("Earlier activity was omitted from the bounded display; use --progress=plain to retain a full log", "omitted", s.DroppedLogs)
	}
	return err
}

func runScreen(ctx context.Context, screen tcell.Screen, tracker *Tracker, opts Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
	ctx, cancel := context.WithCancel(tracker.Context(ctx))
	defer cancel()
	log := slog.New(&logHandler{tracker: tracker, base: logger.Handler()})
	result := make(chan error, 1)
	go func() { result <- work(ctx, log) }()

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
		draw(screen, tracker.Snapshot(), opts, view, time.Now())
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
				screen.Sync()
			case *tcell.EventKey:
				if view.key(event) {
					cancel()
				}
			}
		}
	}
}

type viewState struct {
	logs, warningsOnly, help, canceling, color bool
	offset                                     int
}

func (v *viewState) key(key *tcell.EventKey) bool {
	switch {
	case key.Key() == tcell.KeyCtrlC || key.Rune() == 'q':
		v.canceling = true
		return true
	case key.Rune() == 'l':
		v.logs = !v.logs
	case key.Rune() == 'w':
		v.warningsOnly = !v.warningsOnly
		v.offset = 0
	case key.Rune() == '?':
		v.help = !v.help
	case key.Key() == tcell.KeyEsc:
		v.help = false
	case key.Key() == tcell.KeyUp || key.Rune() == 'k':
		v.offset = min(v.offset+1, logLimit)
	case key.Key() == tcell.KeyDown || key.Rune() == 'j':
		v.offset = max(v.offset-1, 0)
	case key.Key() == tcell.KeyPgUp:
		v.offset = min(v.offset+10, logLimit)
	case key.Key() == tcell.KeyPgDn:
		v.offset = max(v.offset-10, 0)
	case key.Key() == tcell.KeyHome:
		v.offset = logLimit
	case key.Key() == tcell.KeyEnd:
		v.offset = 0
	}
	return false
}
