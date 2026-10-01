//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry/ghcr"
)

// TestHelperProcess is no test of its own: the signal tests run it as a
// child process, running crprune with the arguments after "--". It prints
// "ready" on stderr once crprune handles signals and waits: for its input on
// stdin, which no context cancels, or, with GH_TOKEN set, for the context
// while connecting to GHCR. It prints "terminal restored" where a second
// signal restores the terminal.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CRPRUNE_HELPER_PROCESS") != "1" {
		return
	}
	stdinIsTerminal = func() bool {
		fmt.Fprintln(os.Stderr, "ready")
		return false
	}
	newGHCR = func(ctx context.Context, _ ghcr.Options) (*ghcr.Backend, error) {
		fmt.Fprintln(os.Stderr, "ready")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	restoreTerminal = func() { fmt.Fprintln(os.Stderr, "terminal restored") }
	args := os.Args[slices.Index(os.Args, "--")+1:]
	os.Exit(run(append([]string{"crprune"}, args...)))
}

// output collects what the child process writes to stderr, and closes ready
// once it says it waits.
type output struct {
	mu    sync.Mutex
	text  bytes.Buffer
	ready chan struct{}
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	was := bytes.Contains(o.text.Bytes(), []byte("ready\n"))
	o.text.Write(p)
	if !was && bytes.Contains(o.text.Bytes(), []byte("ready\n")) {
		close(o.ready)
	}
	return len(p), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// startWaiting starts crprune with args in a child process, as
// TestHelperProcess describes, and returns once it waits, with what it writes
// to stderr.
func startWaiting(t *testing.T, env []string, args ...string) (*exec.Cmd, *output) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
	cmd.Env = append(append(os.Environ(), "CRPRUNE_HELPER_PROCESS=1"), env...)
	// Keep stdin open, so that reading it waits.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stderr := &output{ready: make(chan struct{})}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	select {
	case <-stderr.ready:
		return cmd, stderr
	case <-time.After(10 * time.Second):
		t.Fatalf("the child process never waited:\n%s", stderr)
		return nil, nil
	}
}

// wait waits for cmd to end, calling tick every 50ms meanwhile, and fails the
// test if that takes longer than timeout.
func wait(t *testing.T, cmd *exec.Cmd, timeout time.Duration, tick func()) *os.ProcessState {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if _, ok := errors.AsType[*exec.ExitError](err); err != nil && !ok {
				t.Fatal(err)
			}
			return cmd.ProcessState
		case <-ticker.C:
			tick()
		case <-deadline:
			t.Fatalf("the child process did not end within %v", timeout)
		}
	}
}

// TestInterruptedRunExitStatus: a run that a signal cancels reports the
// signal in its exit status, as shells do for a process it killed, rather
// than a plain failure, and says it is stopping.
func TestInterruptedRunExitStatus(t *testing.T) {
	cmd, stderr := startWaiting(t, []string{"GH_TOKEN=token"}, "-r", "ghcr.io/acme", "--progress=plain", "stats")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	state := wait(t, cmd, 10*time.Second, func() {})
	if got, want := state.ExitCode(), 128+int(syscall.SIGTERM); got != want {
		t.Errorf("exit status %v, want %d", state, want)
	}
	if out := stderr.String(); !strings.Contains(out, `msg="Stopping: waiting for requests in flight; another signal exits at once" signal=terminated`) {
		t.Errorf("stderr lacks the stopping message:\n%s", out)
	}
}

// TestSecondSignalEndsProcess: a read of stdin ignores the canceled context,
// and further Ctrl-C used to be ignored as well, leaving only kill -9. Then a
// second signal took the default action, killing the process without
// restoring the terminal or a word about the locks it was restoring. It
// exits at once, restoring the terminal and warning about the locks.
func TestSecondSignalEndsProcess(t *testing.T) {
	for _, tt := range []struct {
		name            string
		args            []string
		stopping, abort string
	}{
		{
			name:     "dry run",
			args:     []string{"prune"},
			stopping: `msg="Stopping: waiting for requests in flight; another signal exits at once" signal=interrupt`,
			abort:    `msg="Exiting at once" signal=interrupt`,
		},
		{
			name:     "deleting locked images",
			args:     []string{"prune", "--dry-run=false", "--include-locked"},
			stopping: "restoring the locks removed for deletions this cuts short; another signal exits at once, abandoning them",
			abort:    "locks being restored may stay removed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, stderr := startWaiting(t, nil, append([]string{"-r", "myreg", "--progress=plain"}, tt.args...)...)
			state := wait(t, cmd, 10*time.Second, func() { _ = cmd.Process.Signal(syscall.SIGINT) })
			if got, want := state.ExitCode(), 128+int(syscall.SIGINT); got != want {
				t.Errorf("child ended with %v, want exit status %d", state, want)
			}
			out := stderr.String()
			stopping, restored, abort := strings.Index(out, tt.stopping), strings.Index(out, "terminal restored"), strings.Index(out, tt.abort)
			if stopping < 0 || restored < stopping || abort < restored {
				t.Errorf("stderr lacks, in order, %q, the terminal restored and %q:\n%s", tt.stopping, tt.abort, out)
			}
		})
	}
}
