package progress

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
)

// logHandler captures structured logs while the TUI owns the terminal, using
// the original logger's level filter. No worker writes terminal escape codes.
type logHandler struct {
	tracker *Tracker
	base    slog.Handler
	attrs   []slog.Attr
	group   string
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *logHandler) Handle(_ context.Context, record slog.Record) error {
	var text strings.Builder
	text.WriteString(record.Message)
	var retryUntil time.Time
	var add func(string, slog.Attr)
	add = func(prefix string, attr slog.Attr) {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			return
		}
		key := prefix + attr.Key
		if attr.Value.Kind() == slog.KindGroup {
			if attr.Key != "" {
				key += "."
			}
			for _, child := range attr.Value.Group() {
				add(key, child)
			}
			return
		}
		if attr.Key == "delay" && attr.Value.Kind() == slog.KindDuration {
			retryUntil = record.Time.Add(attr.Value.Duration())
		}
		fmt.Fprintf(&text, "  %s=%v", key, attr.Value)
	}
	for _, attr := range h.attrs {
		add("", attr)
	}
	record.Attrs(func(attr slog.Attr) bool { add(h.group, attr); return true })
	h.tracker.appendLog(Entry{Time: record.Time, Level: record.Level, Text: truncate(clean(text.String()), maxEntryText)}, retryUntil)
	return nil
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = slices.Clone(h.attrs)
	if h.group != "" {
		for _, attr := range attrs {
			attr.Key = h.group + attr.Key
			clone.attrs = append(clone.attrs, attr)
		}
	} else {
		clone.attrs = append(clone.attrs, attrs...)
	}
	return &clone
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	clone := *h
	if name != "" {
		clone.group += name + "."
	}
	return &clone
}

// maxEntryText bounds the bytes of text an entry keeps, so that the retained
// log stays small whatever is logged: orphan warnings, for one, carry whole
// manifests.
const maxEntryText = 4 << 10

// truncate shortens valid UTF-8 text to at most limit bytes and an ellipsis,
// without splitting a character.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…"
}

// clean prevents control characters and bidi formatting in remote messages
// from changing the meaning or layout of the display or completion summary.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsGraphic(r) {
			return ' '
		}
		return r
	}, s)
}
