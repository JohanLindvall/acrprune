package registry

import (
	"context"
	"log/slog"
	"net/url"
	"time"
)

// The helpers below are shared by the backends' HTTP clients, so that their
// retries behave, and are logged, alike.

// RedactURL returns u for display, without the query, which carries the
// signatures of blob storage URLs, the fragment and any credentials.
func RedactURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment, clean.RawFragment = "", ""
	return clean.String()
}

// Sleep waits for d, or until ctx is done, returning ctx's error then.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LogRetry logs, as a warning, that a request to u is retried after delay,
// and why. The progress display counts the delay attribute down; resume is
// the time of day, on the clock now reads, when the retry is due.
func LogRetry(logger *slog.Logger, method string, u *url.URL, reason string, delay time.Duration, now time.Time) {
	logger.Warn("Retrying request", "method", method, "url", RedactURL(u),
		"reason", reason, "delay", delay, "resume", now.Add(delay).Format(time.TimeOnly))
}
