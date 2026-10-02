package explore

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/gdamore/tcell/v2"
)

type monitoredScreen struct {
	tcell.SimulationScreen
	renders chan string
	finis   atomic.Int32
}

func (s *monitoredScreen) Show() {
	s.SimulationScreen.Show()
	select {
	case s.renders <- screenText(s.SimulationScreen):
	default:
	}
}
func (s *monitoredScreen) Fini() { s.finis.Add(1); s.SimulationScreen.Fini() }
func (s *monitoredScreen) wait(t *testing.T, part string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case text := <-s.renders:
			if strings.Contains(text, part) {
				return
			}
		case <-timer.C:
			t.Fatalf("never rendered %q", part)
		}
	}
}
func fakeScreen(t *testing.T) *monitoredScreen {
	t.Helper()
	s := &monitoredScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), renders: make(chan string, 20)}
	oldNew, oldTerminal := newScreen, stderrIsTerminal
	t.Cleanup(func() { newScreen, stderrIsTerminal = oldNew, oldTerminal })
	newScreen = func() (tcell.Screen, error) { return s, nil }
	stderrIsTerminal = func() bool { return true }
	t.Setenv("TERM", "xterm-256color")
	return s
}
func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("explorer did not stop")
		return nil
	}
}

func TestRunHandlesInputResizeAndRestoresTerminal(t *testing.T) {
	s := fakeScreen(t)
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), sampleStats(), Options{Source: "stats.json"}) }()
	s.wait(t, "STATS EXPLORER")
	s.InjectKey(tcell.KeyRune, '?', tcell.ModNone)
	s.wait(t, "KEYBOARD")
	s.InjectKey(tcell.KeyEsc, 0, tcell.ModNone)
	s.SetSize(40, 10)
	_ = s.PostEvent(tcell.NewEventResize(40, 10))
	s.InjectKey(tcell.KeyRune, '/', tcell.ModNone)
	for _, r := range "no-match" {
		s.InjectKey(tcell.KeyRune, r, tcell.ModNone)
	}
	s.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	s.wait(t, "No matches")
	s.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if s.finis.Load() != 1 {
		t.Fatal("terminal not restored exactly once")
	}
}

func TestQuitWaitsForActiveRequests(t *testing.T) {
	s := fakeScreen(t)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := &Client{Registry: "ghcr.io/test", Connect: func(ctx context.Context, _ *slog.Logger, _ bool) (*registry.Registry, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), sampleStats(), Options{Client: c}) }()
	s.wait(t, "STATS EXPLORER")
	s.InjectKey(tcell.KeyRune, 'm', tcell.ModNone)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	s.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("request was not canceled")
	}
	s.wait(t, "Stopping: waiting")
	if s.finis.Load() != 0 {
		t.Fatal("terminal restored before workers finished")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before cleanup: %v", err)
	default:
	}
	close(release)
	if err := waitRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.finis.Load() != 1 {
		t.Fatal("terminal not restored")
	}
}

func TestForcedRestoreClosesExplorer(t *testing.T) {
	s := fakeScreen(t)
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), sampleStats(), Options{}) }()
	s.wait(t, "STATS EXPLORER")
	tui.RestoreTerminal()
	if err := waitRun(t, done); err == nil {
		t.Fatal("lost terminal did not stop explorer")
	}
	tui.RestoreTerminal()
	if s.finis.Load() != 1 {
		t.Fatal("terminal restored twice")
	}
}

func TestExplorerNeedsTerminal(t *testing.T) {
	fakeScreen(t)
	stderrIsTerminal = func() bool { return false }
	if err := Run(t.Context(), nil, Options{}); err == nil || !strings.Contains(err.Error(), "crprune top") {
		t.Fatal(err)
	}
}
