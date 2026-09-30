// Package progress carries optional batch progress without coupling registry
// operations to a terminal. Updates are cheap, synchronous and concurrency safe.
package progress

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

type Kind uint8

const (
	Candidates Kind = iota
	Repository
	Phase
	Listed
	Fetched
	Cached
	Plan
	Deleted
	Finished
)

// Event describes a batch boundary or completed unit of work. Count and Total
// are stage-specific; Plan carries kept and selected manifest/byte counts.
type Event struct {
	Kind         Kind
	Name         string
	Count, Total int
	Kept         int
	Bytes        uint64
}

type contextKey struct{}

// Report is a no-op unless a tracker is attached to ctx.
func Report(ctx context.Context, event Event) {
	if tracker, ok := ctx.Value(contextKey{}).(*Tracker); ok {
		tracker.report(event)
	}
}

type Entry struct {
	Time  time.Time
	Level slog.Level
	Text  string
}

// Snapshot is a consistent copy of a run's progress and bounded activity log.
type Snapshot struct {
	Started                                  time.Time
	Repository, Phase                        string
	Repositories, Completed, Denied, Skipped int
	Listed, Fetched, Cached                  int
	Kept, Selected, Deleted                  int
	Bytes                                    uint64
	Warnings, DroppedLogs                    int
	RetryUntil                               time.Time
	Logs                                     []Entry
}

const logLimit = 200

type Tracker struct {
	mu sync.Mutex
	s  Snapshot
}

func NewTracker() *Tracker {
	return &Tracker{s: Snapshot{Started: time.Now(), Phase: "Connecting to registry"}}
}

func (t *Tracker) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, t)
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.s
	s.Logs = slices.Clone(s.Logs)
	return s
}

func (t *Tracker) report(e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &t.s
	switch e.Kind {
	case Candidates:
		s.Repositories = e.Total
		s.Phase = "Listing repositories"
	case Repository:
		s.Repository, s.Phase = e.Name, "Inspecting manifests"
		s.Listed, s.Fetched = 0, 0
	case Phase:
		s.Phase = e.Name
	case Listed:
		s.Listed += e.Count
	case Fetched:
		s.Fetched += e.Count
	case Cached:
		s.Cached += e.Count
	case Plan:
		s.Kept += e.Kept
		s.Selected += e.Count
		s.Bytes += e.Bytes
	case Deleted:
		s.Deleted += e.Count
	case Finished:
		s.Completed++
		if e.Name == "denied" {
			s.Denied++
		}
		if e.Name == "skipped" {
			s.Skipped++
		}
	}
}

func (t *Tracker) appendLog(entry Entry, retryUntil time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry.Level >= slog.LevelWarn {
		t.s.Warnings++
	}
	if retryUntil.After(t.s.RetryUntil) {
		t.s.RetryUntil = retryUntil
	}
	if len(t.s.Logs) == logLimit {
		copy(t.s.Logs, t.s.Logs[1:])
		t.s.Logs = t.s.Logs[:logLimit-1]
		t.s.DroppedLogs++
	}
	t.s.Logs = append(t.s.Logs, entry)
}
