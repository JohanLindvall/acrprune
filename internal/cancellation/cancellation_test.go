package cancellation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestOnly(t *testing.T) {
	request := &url.Error{Op: "Get", URL: "https://example.test/manifest", Err: context.Canceled}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, true},
		{"request", fmt.Errorf("failed to prune: %w", request), true},
		{"joined cancellations", errors.Join(request, context.Canceled), true},
		{"unrelated failure", errors.New("failed to restore locks"), false},
		{"same text", errors.New("context canceled"), false},
		{"deadline", context.DeadlineExceeded, false},
		{"canceled and deadline", errors.Join(request, context.DeadlineExceeded), false},
		{"canceled and failure", fmt.Errorf("cleanup: %w", errors.Join(request, errors.New("failed to restore locks"))), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Only(tt.err); got != tt.want {
				t.Fatalf("Only(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
