package ghcr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
)

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 64 << 10

// send performs the request, retrying transient failures and rate limiting,
// and returns the final response whatever its status.
//
// GitHub signals rate limiting with a 403 or 429 and asks clients to wait for
// as long as Retry-After says, until X-RateLimit-Reset when the limit is used
// up, and otherwise at least a minute. Waiting that out is bounded by time
// rather than attempts, so a long cleanup rides out hourly limits instead of
// failing halfway; transient failures get a few attempts with backoff.
func (b *Backend) send(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	req.Header.Set("User-Agent", userAgent)
	failures := 0
	var throttled time.Duration
	for {
		resp, err := b.client.Do(req.Clone(ctx))
		var delay time.Duration
		var reason string
		switch {
		case err != nil:
			err = redactError(err)
			failures++
			if ctx.Err() != nil || failures >= b.maxAttempts {
				return nil, err
			}
			delay, reason = backoff(failures), err.Error()
		case !retryable(resp):
			return resp, nil
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			// retryable only passes a 403 or 429 when it is rate limiting.
			delay = rateLimitDelay(resp, time.Now())
			if throttled+delay > b.maxThrottle {
				return resp, nil
			}
			throttled += delay
			reason = resp.Status
			_ = discard(resp)
		default:
			failures++
			if failures >= b.maxAttempts {
				return resp, nil
			}
			delay, reason = backoff(failures), resp.Status
			_ = discard(resp)
		}
		b.logger.Warn("Retrying request", "method", req.Method, "url", redact(req.URL),
			"reason", reason, "delay", delay, "resume", time.Now().Add(delay).Format(time.TimeOnly))
		if err := b.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// retryable reports whether a response is worth retrying: rate limiting and
// server-side failures are; everything else is final. It buffers the body of a
// 403 or 429, whose message tells rate limiting from missing permission.
func retryable(resp *http.Response) bool {
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return rateLimited(resp, errorMessage(body))
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode >= http.StatusInternalServerError:
		return true
	}
	return false
}

// rateLimited reports whether a response turns a request away for exceeding
// a rate limit, as opposed to for missing permission.
func rateLimited(resp *http.Response, message string) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("Retry-After") != "" ||
			resp.Header.Get("X-RateLimit-Remaining") == "0" ||
			strings.Contains(strings.ToLower(message), "rate limit")
	}
	return false
}

// rateLimitDelay returns how long a rate-limited response asks to wait.
func rateLimitDelay(resp *http.Response, now time.Time) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if seconds, err := strconv.Atoi(s); err == nil {
			return time.Duration(max(seconds, 1)) * time.Second
		}
		if t, err := http.ParseTime(s); err == nil {
			return max(t.Sub(now), time.Second)
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// The primary limit resets hourly; a later reset is bogus.
			return min(max(time.Unix(reset, 0).Sub(now)+time.Second, time.Second), time.Hour)
		}
	}
	return time.Minute
}

// backoff returns an exponentially growing delay for retrying after the
// given number of transient failures.
func backoff(failures int) time.Duration {
	return min(time.Second<<(failures-1), 30*time.Second)
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// check turns a response that is not a success into an error, consuming its
// body. A rate-limited response — returned only once retries are exhausted —
// becomes a plain error rather than a *registry.ResponseError, so it cannot be
// mistaken for missing permission on the resource.
func check(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	message := errorMessage(body)
	status := resp.Status
	if message != "" {
		status += ": " + message
	}
	err := fmt.Errorf("%s %s: %s", resp.Request.Method, redact(resp.Request.URL), status)
	if rateLimited(resp, message) {
		return fmt.Errorf("rate limit exceeded: %w", err)
	}
	return &registry.ResponseError{StatusCode: resp.StatusCode, Err: err}
}

// errorMessage extracts the message from a REST API error ({"message": …}),
// a registry API error ({"errors": [{"code": …, "message": …}]}) or plain
// text.
func errorMessage(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
		Errors  []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Message != "" {
			return parsed.Message
		}
		var messages []string
		for _, e := range parsed.Errors {
			messages = append(messages, strings.TrimSpace(e.Code+" "+e.Message))
		}
		if len(messages) > 0 {
			return strings.Join(messages, "; ")
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// discard drains and closes a response body so the connection can be reused.
func discard(resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return resp.Body.Close()
}

// redact strips the query, which carries signatures on blob storage URLs, and
// any credentials from a URL for display.
func redact(u *url.URL) string {
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	return clean.String()
}

// redactError redacts the URL a failed request names. After a redirect it is
// the blob storage URL, signature and all.
func redactError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	clean := *urlErr
	if u, parseErr := url.Parse(urlErr.URL); parseErr == nil {
		clean.URL = redact(u)
	} else {
		clean.URL = "(unparsable URL)"
	}
	return &clean
}
