package registrytest

import (
	"context"
	"log/slog"
	"sync"
)

// Recorder is a slog.Handler keeping every record it handles, whatever its
// level, for tests to inspect what was logged. It is safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

// Enabled reports that every level is recorded.
func (r *Recorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle records a copy of the record.
func (r *Recorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
	return nil
}

// WithAttrs returns r itself: the attributes are not recorded.
func (r *Recorder) WithAttrs([]slog.Attr) slog.Handler { return r }

// WithGroup returns r itself: the group is not recorded.
func (r *Recorder) WithGroup(string) slog.Handler { return r }

// Records returns the records handled so far.
func (r *Recorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]slog.Record, len(r.records))
	for i, record := range r.records {
		records[i] = record.Clone()
	}
	return records
}

// Messages returns the records logged at level, each as its message followed
// by its attributes.
func (r *Recorder) Messages(level slog.Level) []string {
	var result []string
	for _, record := range r.Records() {
		if record.Level != level {
			continue
		}
		text := record.Message
		record.Attrs(func(a slog.Attr) bool {
			text += " " + a.String()
			return true
		})
		result = append(result, text)
	}
	return result
}
