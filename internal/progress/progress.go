// Package progress carries optional batch progress without coupling registry
// operations to a terminal. Updates are cheap, synchronous and concurrency safe.
package progress

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Kind identifies what an Event reports, and so which of its fields count.
type Kind uint8

const (
	// Candidates announces how many repositories the run will process, in
	// Total.
	Candidates Kind = iota
	// Repository starts the repository named by Name. It resets the
	// per-repository Listed, Fetched and Cached counts.
	Repository
	// Phase names in Name what the current repository is going through.
	Phase
	// Listed counts Count manifests listed in the current repository.
	Listed
	// Fetched counts Count manifest documents obtained for the current
	// repository, whether downloaded or read from the cache.
	Fetched
	// Cached counts Count documents the current repository read from the
	// local cache instead of downloading them.
	Cached
	// Plan reports a repository's decision: Kept manifests kept, Count
	// selected for deletion, and Bytes the estimated size removable. A
	// statistics scan reports the scanned manifests in Kept and their
	// unique bytes in Bytes instead.
	Plan
	// Deleted counts Count manifests actually deleted.
	Deleted
	// Finished ends the current repository with Outcome.
	Finished
)

var kindNames = [...]string{"Candidates", "Repository", "Phase", "Listed", "Fetched", "Cached", "Plan", "Deleted", "Finished"}

// String returns the name of the kind, for debugging.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", k)
}

// Outcome is how a Finished repository ended.
type Outcome uint8

const (
	// OK means the repository was processed.
	OK Outcome = iota
	// Denied means the credential may not access the repository, which was
	// skipped.
	Denied
	// Skipped means the repository was left untouched, for example because
	// it was missing or some of its manifests could not be downloaded.
	Skipped
)

// Event describes a batch boundary or completed unit of work. Which fields
// count depends on Kind; its constants say which each one reads.
type Event struct {
	Kind         Kind
	Name         string
	Count, Total int
	Kept         int
	Bytes        uint64
	Outcome      Outcome
}

type contextKey struct{}

// Report is a no-op unless a tracker is attached to ctx.
func Report(ctx context.Context, event Event) {
	if tracker, ok := ctx.Value(contextKey{}).(*Tracker); ok {
		tracker.report(event)
	}
}

// Entry is a log record captured for the activity panel, with its attributes
// flattened into Text.
type Entry struct {
	Time  time.Time
	Level slog.Level
	Text  string
}

// Snapshot is a consistent copy of a run's progress and bounded activity log.
// Listed, Fetched and Cached count the current repository; the other counters
// cover the whole run.
type Snapshot struct {
	Started                                  time.Time
	Repository, Phase                        string
	Repositories, Completed, Denied, Skipped int
	Listed, Fetched, Cached                  int
	Kept, Selected, Deleted                  int
	Bytes                                    uint64
	Warnings                                 int
	RetryUntil                               time.Time
	// Logs holds the latest entries of every level. WarningLogs holds the
	// latest entries at warning level or above on their own, so that a busy
	// run's info and debug lines do not push them out. DroppedLogs and
	// DroppedWarnings count the entries each has discarded.
	Logs, WarningLogs            []Entry
	DroppedLogs, DroppedWarnings int
}

const (
	// logLimit and warningLimit bound the entries a Snapshot retains.
	logLimit     = 200
	warningLimit = 100
)

// Tracker accumulates the events reported through its Context and the log
// entries of a run.
type Tracker struct {
	mu sync.Mutex
	s  Snapshot
}

// NewTracker returns a tracker for a run starting now.
func NewTracker() *Tracker {
	return &Tracker{s: Snapshot{Started: time.Now(), Phase: "Connecting to registry"}}
}

// Reset starts another operation using the same captured logger. Call after
// the previous operation's workers have stopped.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s = Snapshot{Started: time.Now(), Phase: "Connecting to registry"}
}

// Context returns a context whose Report calls update t.
func (t *Tracker) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, t)
}

// Snapshot returns a copy of the progress so far, sharing no storage with t.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.s
	s.Logs = slices.Clone(s.Logs)
	s.WarningLogs = slices.Clone(s.WarningLogs)
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
		s.Listed, s.Fetched, s.Cached = 0, 0, 0
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
		switch e.Outcome {
		case Denied:
			s.Denied++
		case Skipped:
			s.Skipped++
		}
	}
}

func (t *Tracker) appendLog(entry Entry, retryUntil time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if retryUntil.After(t.s.RetryUntil) {
		t.s.RetryUntil = retryUntil
	}
	var dropped bool
	if t.s.Logs, dropped = appendBounded(t.s.Logs, entry, logLimit); dropped {
		t.s.DroppedLogs++
	}
	if entry.Level >= slog.LevelWarn {
		t.s.Warnings++
		if t.s.WarningLogs, dropped = appendBounded(t.s.WarningLogs, entry, warningLimit); dropped {
			t.s.DroppedWarnings++
		}
	}
}

// appendBounded appends entry to entries, first dropping the oldest entry when
// there are already limit of them, and reports whether it did.
func appendBounded(entries []Entry, entry Entry, limit int) ([]Entry, bool) {
	dropped := len(entries) == limit
	if dropped {
		copy(entries, entries[1:])
		entries = entries[:limit-1]
	}
	return append(entries, entry), dropped
}
