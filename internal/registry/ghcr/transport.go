package ghcr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
// up, and otherwise at least a minute, exponentially longer while the limit
// persists. One rate-limited response pauses all of the backend's requests,
// so that parallel workers do not each run into the limit again. Waiting that
// out is bounded by time rather than attempts, so a long cleanup rides out
// hourly limits instead of failing halfway; transient failures get a few
// attempts with backoff.
func (b *Backend) send(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	req.Header.Set("User-Agent", userAgent)
	client := registry.NewHTTPClient(b.client)
	failures := 0
	var throttled time.Duration
	for {
		for wait := b.throttle.pause(b.now()); wait > 0; wait = b.throttle.pause(b.now()) {
			if wait > b.maxThrottle-throttled {
				return nil, fmt.Errorf("rate limit exceeded: %s %s would wait %s more for GitHub's rate limit to lift", req.Method, registry.RedactURL(req.URL), wait.Round(time.Second))
			}
			throttled += wait
			if err := b.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		sent := b.now()
		resp, err := client.Do(req.Clone(ctx))
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			err = registry.BufferResponse(resp)
		}
		var delay time.Duration
		var reason string
		switch {
		case err != nil:
			err = registry.RedactError(err)
			failures++
			if !registry.Transient(err) || ctx.Err() != nil || failures >= b.maxAttempts {
				return nil, err
			}
			delay, reason = backoff(failures), err.Error()
		case !retryable(resp):
			b.throttle.passed(sent)
			return resp, nil
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			// retryable only passes a 403 or 429 when it is rate limiting.
			delay = b.throttle.limited(resp, sent, b.now(), b.maxThrottle)
			if delay > b.maxThrottle-throttled {
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
		registry.LogRetry(b.logger, req.Method, req.URL, reason, delay, b.now())
		if err := b.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// throttle is the rate limiting a backend's requests share: when GitHub turns
// one away, they all wait until the limit lifts instead of each running into
// it. A secondary limit that comes without saying how long to wait is waited
// out exponentially longer each time it persists.
type throttle struct {
	mu sync.Mutex
	// until is when requests may resume, and hit when rate limiting was
	// last noticed.
	until, hit time.Time
	// streak counts the unguided limits hit since a request last got
	// through.
	streak int
}

// pause returns how long requests must still wait at now.
func (t *throttle) pause(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.until.Sub(now)
}

// limited records a rate-limited response to a request sent at sent, and
// returns how long from now to wait before retrying it. A response to a
// request sent before the latest limit was noticed is part of the same burst,
// which the pause already covers unless the response asks for longer. Unguided
// waits are capped at maxDelay.
func (t *throttle) limited(resp *http.Response, sent, now time.Time, maxDelay time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	burst := sent.Before(t.hit)
	delay, guided := rateLimitDelay(resp, now)
	switch {
	case guided:
	case burst:
		delay = 0
	default:
		t.streak++
		delay = min(delay<<min(t.streak-1, 16), maxDelay)
	}
	if !burst {
		t.hit = now
	}
	if resume := now.Add(delay); resume.After(t.until) {
		t.until = resume
	}
	return max(t.until.Sub(now), 0)
}

// passed records that a request sent at sent got through, so that an unguided
// limit hit after it waits the shortest time again.
func (t *throttle) passed(sent time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !sent.Before(t.hit) {
		t.streak = 0
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

// rateLimitDelay returns how long a rate-limited response asks to wait, and
// whether it says at all; otherwise it returns a minute. The times a response
// names are measured against its Date header when it has one, so that a local
// clock running ahead does not cut the wait short.
func rateLimitDelay(resp *http.Response, now time.Time) (time.Duration, bool) {
	if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		now = date
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if seconds, err := strconv.ParseInt(s, 10, 64); err == nil {
			if seconds > math.MaxInt64/int64(time.Second) {
				return time.Duration(math.MaxInt64), true
			}
			return time.Duration(max(seconds, 1)) * time.Second, true
		}
		if t, err := http.ParseTime(s); err == nil {
			return max(t.Sub(now), time.Second), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// The primary limit resets hourly; a later reset is bogus.
			return min(max(time.Unix(reset, 0).Sub(now), 0), time.Hour-time.Second) + time.Second, true
		}
	}
	return time.Minute, false
}

// backoff returns an exponentially growing delay for retrying after the
// given number of transient failures.
func backoff(failures int) time.Duration {
	return min(time.Second<<min(max(failures-1, 0), 5), 30*time.Second)
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
	err := fmt.Errorf("%s %s: %s", resp.Request.Method, registry.RedactURL(resp.Request.URL), status)
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

// redactURL is registry.RedactURL for a URL in text form.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(unparsable URL)"
	}
	return registry.RedactURL(u)
}
