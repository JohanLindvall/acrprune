package main

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/JohanLindvall/crprune/internal/progress"
)

// Seams for tests: restoring the terminal from the progress display, and
// exiting the process, on a second signal.
var (
	restoreTerminal = progress.RestoreTerminal
	exit            = os.Exit
)

// signalCause is the cause of a run's cancellation by a signal.
type signalCause struct {
	os.Signal
}

// Error names the signal.
func (c signalCause) Error() string {
	return c.String() + " signal received"
}

// signalStatus returns the exit status shells report for a process the
// signal ended: 128 plus its number.
func signalStatus(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}

// interrupts handles the SIGINT and SIGTERM a run receives. The first cancels
// the run, which winds down, restoring the locks it removed for deletions it
// cuts short, and reports what it did. A second exits at once, also while the
// run waits for stdin, which no context cancels; it restores the terminal
// first if the progress display owns it.
type interrupts struct {
	// base logs to stderr, and display, while set, on the progress display,
	// so that messages show there instead of across it.
	base, display atomic.Pointer[slog.Logger]
	// unlocking is set when the run removes locks to delete, so that the
	// messages say what an interruption means for them.
	unlocking atomic.Bool
}

type interruptsKey struct{}

// withInterrupts returns ctx carrying in, for the commands to tell it about
// their logging and locks.
func withInterrupts(ctx context.Context, in *interrupts) context.Context {
	return context.WithValue(ctx, interruptsKey{}, in)
}

// interruptsOf returns the interrupts ctx carries, or nil.
func interruptsOf(ctx context.Context) *interrupts {
	in, _ := ctx.Value(interruptsKey{}).(*interrupts)
	return in
}

// logger returns where to report a signal: on the progress display while it
// runs, and otherwise to stderr.
func (in *interrupts) logger() *slog.Logger {
	if logger := in.display.Load(); logger != nil {
		return logger
	}
	if logger := in.base.Load(); logger != nil {
		return logger
	}
	return slog.Default()
}

// handle cancels the run on the first signal and exits on the second, until
// done is closed.
func (in *interrupts) handle(signals <-chan os.Signal, done <-chan struct{}, cancel context.CancelCauseFunc) {
	var sig os.Signal
	select {
	case sig = <-signals:
	case <-done:
		return
	}
	cancel(signalCause{sig})
	if in.unlocking.Load() {
		in.logger().Warn("Stopping: waiting for requests in flight and restoring the locks removed for deletions this cuts short; another signal exits at once, abandoning them", "signal", sig.String())
	} else {
		in.logger().Warn("Stopping: waiting for requests in flight; another signal exits at once", "signal", sig.String())
	}
	select {
	case sig = <-signals:
	case <-done:
		return
	}
	restoreTerminal()
	logger := in.base.Load()
	if logger == nil {
		logger = slog.Default()
	}
	if in.unlocking.Load() {
		logger.Warn("Exiting at once; locks being restored may stay removed: check the locks of the manifests the run was deleting", "signal", sig.String())
	} else {
		logger.Warn("Exiting at once", "signal", sig.String())
	}
	exit(signalStatus(sig))
}

// display runs work under the progress display, as runProgress does, with
// signals reported on it.
func display(ctx context.Context, opts progress.Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
	return runProgress(ctx, opts, logger, func(ctx context.Context, logger *slog.Logger) error {
		if in := interruptsOf(ctx); in != nil {
			in.display.Store(logger)
			defer in.display.Store(nil)
		}
		return work(ctx, logger)
	})
}
