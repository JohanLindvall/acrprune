package registry_test

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
)

func TestRedactURL(t *testing.T) {
	u, err := url.Parse("https://user:pass@storage.example/blob?signature=secret#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.RedactURL(u); got != "https://storage.example/blob" {
		t.Errorf("RedactURL = %q", got)
	}
	if u.User == nil || u.RawQuery == "" {
		t.Error("RedactURL changed its argument")
	}
}

func TestSleep(t *testing.T) {
	if err := registry.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("Sleep = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := registry.Sleep(canceled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep with a canceled context = %v", err)
	}
}

// TestLogRetry: the progress display counts down the delay attribute, which
// must stay a duration, and the URL must not leak credentials.
func TestLogRetry(t *testing.T) {
	logs := &registrytest.Recorder{}
	u, _ := url.Parse("https://user:pass@host/v2/app?token=secret")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	registry.LogRetry(slog.New(logs), "GET", u, "503 Service Unavailable", 90*time.Second, now)
	records := logs.Records()
	if len(records) != 1 || records[0].Level != slog.LevelWarn || records[0].Message != "Retrying request" {
		t.Fatalf("records = %v, want one retry warning", records)
	}
	attrs := map[string]slog.Value{}
	records[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	if attrs["url"].String() != "https://host/v2/app" || attrs["method"].String() != "GET" || attrs["reason"].String() != "503 Service Unavailable" {
		t.Errorf("attributes = %v", attrs)
	}
	if delay := attrs["delay"]; delay.Kind() != slog.KindDuration || delay.Duration() != 90*time.Second {
		t.Errorf("delay = %v, want a duration of 90s", delay)
	}
	if got := attrs["resume"].String(); got != "03:05:35" {
		t.Errorf("resume = %q, want the time of day the retry is due", got)
	}
}
