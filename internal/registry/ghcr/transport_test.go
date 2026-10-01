package ghcr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
)

func TestRetriesTruncatedSuccessBodies(t *testing.T) {
	for _, operation := range []string{"listing", "manifest"} {
		t.Run(operation, func(t *testing.T) {
			f, b := newFakeGitHub(t, true)
			digest := f.push("app", manifestDoc("a"), time.Now(), "v1")
			sleeps := recordSleeps(b)
			var failed atomic.Bool
			f.before = func(w http.ResponseWriter, r *http.Request) bool {
				path := "/orgs/" + testOwner + "/packages"
				if operation == "manifest" {
					path = "/v2/" + testOwner + "/app/manifests/" + digest
				}
				if r.URL.Path != path || r.Header.Get("Authorization") == "" || failed.Swap(true) {
					return false
				}
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "{")
				return true
			}
			var err error
			if operation == "manifest" {
				_, err = b.GetManifest(t.Context(), "app", digest)
			} else {
				_, err = b.ListRepositories(t.Context())
			}
			if err != nil || len(*sleeps) != 1 {
				t.Fatalf("error = %v, waits = %v; want the truncated body retried", err, *sleeps)
			}
		})
	}
}

// failFirst makes the fake answer the first n REST requests with fail.
func failFirst(f *fakeGitHub, n int, fail func(w http.ResponseWriter)) {
	fails := make([]func(http.ResponseWriter), n)
	for i := range fails {
		fails[i] = fail
	}
	failSequence(f, fails...)
}

// failSequence makes the fake answer its next REST requests with fails, in
// order.
func failSequence(f *fakeGitHub, fails ...func(w http.ResponseWriter)) {
	var count atomic.Int32
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/token" {
			return false
		}
		i := int(count.Add(1)) - 1
		if i >= len(fails) {
			return false
		}
		fails[i](w)
		return true
	}
}

func rateLimit(retryAfter string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", retryAfter)
		apiError(w, http.StatusForbidden, "You have exceeded a secondary rate limit.")
	}
}

func serverError(w http.ResponseWriter) {
	apiError(w, http.StatusBadGateway, "Server Error")
}

func TestRetriesServerErrors(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	sleeps := recordSleeps(b)
	failFirst(f, 2, func(w http.ResponseWriter) { apiError(w, http.StatusBadGateway, "Server Error") })

	names, err := b.ListRepositories(ctx)
	if err != nil || !slices.Equal(names, []string{"app"}) {
		t.Fatalf("ListRepositories = %v, %v", names, err)
	}
	if !slices.Equal(*sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("backoff = %v, want 1s then 2s", *sleeps)
	}
}

func TestRetriesRateLimits(t *testing.T) {
	reset := time.Now().Add(90 * time.Second).Unix()
	tests := []struct {
		name  string
		fail  func(w http.ResponseWriter)
		check func(time.Duration) bool
	}{
		{"secondary limit with Retry-After", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "7")
			apiError(w, http.StatusForbidden, "You have exceeded a secondary rate limit.")
		}, func(d time.Duration) bool { return d == 7*time.Second }},
		{"secondary limit without headers", func(w http.ResponseWriter) {
			apiError(w, http.StatusForbidden, "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
		}, func(d time.Duration) bool { return d == time.Minute }},
		{"primary limit used up", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			apiError(w, http.StatusForbidden, "API rate limit exceeded for user ID 1.")
		}, func(d time.Duration) bool { return d > 80*time.Second && d <= 92*time.Second }},
		{"too many requests", func(w http.ResponseWriter) {
			apiError(w, http.StatusTooManyRequests, "Too Many Requests")
		}, func(d time.Duration) bool { return d == time.Minute }},
	}
	for _, tt := range tests {
		f, b := newFakeGitHub(t, true)
		f.push("app", manifestDoc("a"), time.Now(), "v1")
		sleeps := recordSleeps(b)
		failFirst(f, 1, tt.fail)

		if _, err := b.ListRepositories(ctx); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if len(*sleeps) != 1 || !tt.check((*sleeps)[0]) {
			t.Errorf("%s: waited %v", tt.name, *sleeps)
		}
	}
}

// TestRateLimitExhaustion: a rate limit outlasting the time a request may
// wait fails it, and is not mistaken for missing permission — which would
// skip the repository as denied.
func TestRateLimitExhaustion(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	sleeps := recordSleeps(b)
	b.maxThrottle = 5 * time.Second
	failFirst(f, 1000, rateLimit("1"))

	_, err := b.ListRepositories(ctx)
	if err == nil || registry.IsPermissionError(err) || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("error = %v, want a rate limit error that is not a permission error", err)
	}
	if len(*sleeps) != 5 {
		t.Errorf("waited %v, want five one-second waits within the five-second budget", *sleeps)
	}
}

// TestRateLimitsOutlastAttempts: a long cleanup keeps hitting GitHub's hourly
// limits; waiting them out is bounded by time, not by the attempts allowed
// for transient failures, and does not use those attempts up.
func TestRateLimitsOutlastAttempts(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	sleeps := recordSleeps(b)
	var fails []func(http.ResponseWriter)
	for range 2 * b.maxAttempts {
		fails = append(fails, rateLimit("60"))
	}
	for range b.maxAttempts - 1 {
		fails = append(fails, serverError)
	}
	failSequence(f, fails...)

	if _, err := b.ListRepositories(ctx); err != nil {
		t.Fatalf("rate limits and a few server errors should be waited out: %v", err)
	}
	if len(*sleeps) != len(fails) || (*sleeps)[0] != time.Minute || (*sleeps)[len(fails)-1] != 16*time.Second {
		t.Errorf("waits = %v", *sleeps)
	}
}

// TestPermissionErrorsAreFinal: a 403 that is not rate limiting is not
// retried.
func TestPermissionErrorsAreFinal(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	failFirst(f, 1, func(w http.ResponseWriter) {
		apiError(w, http.StatusForbidden, "Must have admin rights to Repository.")
	})
	_, err := b.ListRepositories(ctx) // the fake's unexpected-retry guard fails the test on a retry
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "admin rights") {
		t.Errorf("error = %v, want the permission error", err)
	}
}

func TestRetriesNetworkErrors(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	sleeps := recordSleeps(b)
	// The transport itself retries a request that fails on a reused
	// connection; on fresh connections the failure reaches the backend.
	b.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	var dropped atomic.Bool
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if dropped.Swap(true) {
			return false
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close() // drop the connection without a response
		return true
	}
	if _, err := b.ListRepositories(ctx); err != nil {
		t.Fatalf("a dropped connection should be retried: %v", err)
	}
	if len(*sleeps) != 1 {
		t.Errorf("retries = %v, want one", *sleeps)
	}
}

func TestRetryStopsWithContext(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	failFirst(f, 1000, func(w http.ResponseWriter) { apiError(w, http.StatusServiceUnavailable, "unavailable") })
	cancelled, cancel := context.WithCancel(ctx)
	b.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return registry.Sleep(ctx, d)
	}
	if _, err := b.ListRepositories(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
}

// TestRateLimited: each of GitHub's signals marks a response as rate limited
// on its own; a 403 carrying none of them is missing permission.
func TestRateLimited(t *testing.T) {
	tests := []struct {
		status  int
		headers []string
		message string
		want    bool
	}{
		{http.StatusTooManyRequests, nil, "", true},
		{http.StatusForbidden, []string{"Retry-After", "5"}, "Forbidden", true},
		{http.StatusForbidden, []string{"X-RateLimit-Remaining", "0"}, "Forbidden", true},
		{http.StatusForbidden, nil, "You have exceeded a secondary rate limit.", true},
		{http.StatusForbidden, nil, "API rate limit exceeded for user ID 1.", true},
		{http.StatusForbidden, []string{"X-RateLimit-Remaining", "4999"}, "Must have admin rights to Repository.", false},
		{http.StatusForbidden, nil, "Resource not accessible by integration", false},
		{http.StatusUnauthorized, []string{"Retry-After", "5"}, "rate limit", false},
		{http.StatusInternalServerError, nil, "rate limit", false},
	}
	for _, tt := range tests {
		resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
		for i := 0; i < len(tt.headers); i += 2 {
			resp.Header.Set(tt.headers[i], tt.headers[i+1])
		}
		if got := rateLimited(resp, tt.message); got != tt.want {
			t.Errorf("rateLimited(%d, %v, %q) = %v, want %v", tt.status, tt.headers, tt.message, got, tt.want)
		}
	}
}

func TestRateLimitDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	response := func(status int, headers ...string) *http.Response {
		resp := &http.Response{StatusCode: status, Header: http.Header{}}
		for i := 0; i < len(headers); i += 2 {
			resp.Header.Set(headers[i], headers[i+1])
		}
		return resp
	}
	// GitHub's clock is ten minutes behind the local one.
	serverNow := now.Add(-10 * time.Minute)
	serverDate := serverNow.Format(http.TimeFormat)
	tests := []struct {
		name   string
		resp   *http.Response
		want   time.Duration
		guided bool
	}{
		{"Retry-After seconds", response(http.StatusForbidden, "Retry-After", "30"), 30 * time.Second, true},
		{"Retry-After zero waits a second", response(http.StatusTooManyRequests, "Retry-After", "0"), time.Second, true},
		{"Retry-After date", response(http.StatusTooManyRequests, "Retry-After", now.Add(45*time.Second).Format(http.TimeFormat)), 45 * time.Second, true},
		{"Retry-After date on the server's clock", response(http.StatusTooManyRequests, "Date", serverDate, "Retry-After", serverNow.Add(45*time.Second).Format(http.TimeFormat)), 45 * time.Second, true},
		{"reset", response(http.StatusForbidden, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Minute).Unix(), 10)), time.Minute + time.Second, true},
		{"reset on the server's clock", response(http.StatusForbidden, "Date", serverDate, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(serverNow.Add(time.Minute).Unix(), 10)), time.Minute + time.Second, true},
		{"unparsable date", response(http.StatusForbidden, "Date", "yesterday", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Minute).Unix(), 10)), time.Minute + time.Second, true},
		{"bogus reset is capped", response(http.StatusForbidden, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10)), time.Hour, true},
		{"past reset waits a second", response(http.StatusForbidden, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)), time.Second, true},
		{"no guidance waits a minute", response(http.StatusForbidden, "Date", serverDate), time.Minute, false},
	}
	for _, tt := range tests {
		if got, guided := rateLimitDelay(tt.resp, now); got != tt.want || guided != tt.guided {
			t.Errorf("%s: rateLimitDelay = %v, %v; want %v, %v", tt.name, got, guided, tt.want, tt.guided)
		}
	}
}

// TestThrottle: an unguided limit is waited out exponentially longer while it
// persists, up to a cap, but not for every response of one burst of requests;
// a request getting through starts over.
func TestThrottle(t *testing.T) {
	var th throttle
	secondary := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	if got := th.limited(secondary, at(0), at(time.Second), time.Hour); got != time.Minute {
		t.Fatalf("first wait = %v, want a minute", got)
	}
	// Sent together with the first, answered a little later.
	if got := th.limited(secondary, at(0), at(2*time.Second), time.Hour); got != time.Minute-time.Second || th.streak != 1 {
		t.Fatalf("same burst: wait %v, streak %d; want the rest of the pause", got, th.streak)
	}
	if got := th.pause(at(31 * time.Second)); got != 30*time.Second {
		t.Fatalf("pause = %v, want the 30s left", got)
	}
	if got := th.limited(secondary, at(time.Minute+time.Second), at(time.Minute+2*time.Second), time.Hour); got != 2*time.Minute {
		t.Fatalf("persisting limit: wait %v, want two minutes", got)
	}
	for i := range 10 {
		now := at(time.Duration(i+2) * time.Hour)
		th.limited(secondary, now, now, time.Hour)
	}
	if got := th.limited(secondary, at(20*time.Hour), at(20*time.Hour), time.Hour); got != time.Hour {
		t.Fatalf("wait = %v, want it capped at an hour", got)
	}
	th.passed(at(19 * time.Hour)) // sent before the latest limit
	if th.streak == 0 {
		t.Fatal("a request sent before the latest limit does not show that it lifted")
	}
	th.passed(at(21 * time.Hour))
	if got := th.limited(secondary, at(22*time.Hour), at(22*time.Hour), time.Hour); got != time.Minute {
		t.Fatalf("wait after a request got through = %v, want a minute", got)
	}
	guided := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Retry-After": {"5"}}}
	if got := th.limited(guided, at(23*time.Hour), at(23*time.Hour), time.Hour); got != 5*time.Second || th.streak != 1 {
		t.Fatalf("guided wait = %v, streak %d", got, th.streak)
	}
}

// TestRateLimitPausesEveryRequest: a request starting while another waits
// out a rate limit waits too, instead of running into the limit itself.
func TestRateLimitPausesEveryRequest(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	sleeps := recordSleeps(b)
	failFirst(f, 1, rateLimit("30"))
	record := b.sleep
	var other error
	started := false
	b.sleep = func(ctx context.Context, d time.Duration) error {
		if !started {
			// The first wait: start another request before any time passes.
			started = true
			_, other = b.ListRepositories(ctx)
		}
		return record(ctx, d)
	}

	if _, err := b.ListRepositories(ctx); err != nil || other != nil {
		t.Fatalf("errors: %v, %v", err, other)
	}
	if !slices.Equal(*sleeps, []time.Duration{30 * time.Second, 30 * time.Second}) {
		t.Errorf("waits = %v, want both requests to wait for the limit to lift", *sleeps)
	}
	// The fake records the listings it serves, not the one it turned away.
	if got := f.requested("GET /orgs/" + testOwner + "/packages"); len(got) != 2 {
		t.Errorf("listings = %v, want one from each request after the pause", got)
	}
}

// TestRateLimitPauseOutlastingBudget: a request that would have to wait for
// the shared pause longer than it may wait fails without being sent.
func TestRateLimitPauseOutlastingBudget(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	recordSleeps(b)
	b.throttle.until = b.now().Add(3 * time.Hour)
	_, err := b.ListRepositories(ctx)
	if err == nil || registry.IsPermissionError(err) || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("error = %v, want a rate limit error", err)
	}
	if got := f.requested("GET /orgs/"); len(got) != 0 {
		t.Errorf("requests sent during the pause: %v", got)
	}

	// Waiting for the pause to end stops with the request's context.
	b.throttle.until = b.now().Add(time.Minute)
	b.sleep = registry.Sleep
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.ListRepositories(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
}

func TestRateLimitRechecksAnExtendedPause(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	recordSleeps(b)
	b.throttle.until = b.now().Add(time.Minute)
	sleep := b.sleep
	waits := 0
	b.sleep = func(ctx context.Context, d time.Duration) error {
		waits++
		if waits == 1 {
			// Another in-flight response extends the shared pause while we wait.
			b.throttle.until = b.now().Add(2 * time.Minute)
		}
		return sleep(ctx, d)
	}
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if b.throttle.pause(b.now()) > 0 {
			t.Error("request sent before the extended rate-limit pause ended")
		}
		return false
	}
	if _, err := b.ListRepositories(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waits != 2 {
		t.Errorf("waited %d times, want the original pause and its extension", waits)
	}
}

// TestSecondaryRateLimitBackoff: GitHub asks to wait exponentially longer
// while a secondary limit persists without saying how long.
func TestSecondaryRateLimitBackoff(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	sleeps := recordSleeps(b)
	secondary := func(w http.ResponseWriter) {
		apiError(w, http.StatusForbidden, "You have exceeded a secondary rate limit.")
	}
	failFirst(f, 4, secondary)
	if _, err := b.ListRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	failFirst(f, 1, secondary)
	if _, err := b.ListRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, time.Minute}
	if !slices.Equal(*sleeps, want) {
		t.Errorf("waits = %v, want %v", *sleeps, want)
	}
}

// TestServerErrorsExhausted: a server error persisting through every attempt
// fails the request with its status, which is not a permission error.
func TestServerErrorsExhausted(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	sleeps := recordSleeps(b)
	failFirst(f, 1000, serverError)
	_, err := b.ListRepositories(ctx)
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusBadGateway || registry.IsPermissionError(err) {
		t.Errorf("error = %v, want a 502 response error", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if !slices.Equal(*sleeps, want) {
		t.Errorf("backoff = %v, want %v", *sleeps, want)
	}
}

func TestBackoff(t *testing.T) {
	var got []time.Duration
	for attempt := 1; attempt <= 7; attempt++ {
		got = append(got, backoff(attempt))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("backoff = %v, want %v", got, want)
	}
}

func TestErrorMessage(t *testing.T) {
	tests := []struct{ body, want string }{
		{`{"message": "Bad credentials", "documentation_url": "x"}`, "Bad credentials"},
		{`{"errors": [{"code": "DENIED", "message": "denied"}, {"code": "UNAUTHORIZED", "message": "no"}]}`, "DENIED denied; UNAUTHORIZED no"},
		{"plain text\n", "plain text"},
		{"", ""},
		{strings.Repeat("x", 300), strings.Repeat("x", 200) + "…"},
	}
	for _, tt := range tests {
		if got := errorMessage([]byte(tt.body)); got != tt.want {
			t.Errorf("errorMessage(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

// TestFailedRedirectRedactsSignature: a blob download failing at the storage
// URLs ghcr.io redirects to must not log or return their signatures.
func TestFailedRedirectRedactsSignature(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	digest := f.pushBlob([]byte("{}"))
	sleeps := recordSleeps(b)
	var logs bytes.Buffer
	b.logger = slog.New(slog.NewTextHandler(&logs, nil))
	b.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/cdn/") {
			return false
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close() // drop the connection without a response
		}
		return true
	}

	_, err := b.GetBlob(ctx, "app", digest)
	if err == nil || !strings.Contains(err.Error(), "/cdn/"+digest) {
		t.Fatalf("error = %v, want the failed storage request", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(logs.String(), "secret") {
		t.Errorf("the signature leaked:\nerror: %v\nlogs: %s", err, logs.String())
	}
	if len(*sleeps) != b.maxAttempts-1 {
		t.Errorf("retries = %v", *sleeps)
	}
}

// TestBlobRedirectDropsAuthorization: blob storage lives on another host than
// ghcr.io, which must not see the registry token.
func TestBlobRedirectDropsAuthorization(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	f.push("app", manifestDoc("a"), time.Now(), "v1")
	content := []byte(`{"architecture":"arm64","os":"linux"}`)
	digest := f.pushBlob(content)

	var authorization atomic.Pointer[string]
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		authorization.Store(&header)
		_, _ = w.Write(content)
	}))
	defer storage.Close()
	_, port, err := net.SplitHostPort(storage.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Redirect to a host name of its own, which the client resolves to the
	// storage server.
	f.blobStorage = "http://storage.test:" + port
	var dialer net.Dialer
	b.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "storage.test:") {
				addr = storage.Listener.Addr().String()
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}

	got, err := b.GetBlob(ctx, "app", digest)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("GetBlob = %q, %v", got, err)
	}
	seen := authorization.Load()
	if seen == nil {
		t.Fatal("the storage server was not reached")
	}
	if *seen != "" {
		t.Errorf("the storage host received Authorization %q", *seen)
	}
}

// TestNextPage: next page links carry the token, so they must stay on the
// API's origin.
func TestNextPage(t *testing.T) {
	apiURL, _ := url.Parse("https://api.github.com")
	b := &Backend{apiURL: apiURL}
	tests := []struct {
		link, want string
		ok         bool
	}{
		{`<https://api.github.com/orgs/o/packages?page=2>; rel="next", <https://api.github.com/orgs/o/packages?page=5>; rel="last"`, "https://api.github.com/orgs/o/packages?page=2", true},
		{`<https://api.github.com/x?page=4>; rel="prev", <https://api.github.com/x?page=1>; rel="first"`, "", true},
		{``, "", true},
		{`<https://evil.example/steal>; rel="next"`, "", false},
		{`<http://api.github.com/x?page=2>; rel="next"`, "", false},
	}
	for _, tt := range tests {
		got, err := b.nextPage(tt.link)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("nextPage(%q) = %q, %v; want %q, ok=%v", tt.link, got, err, tt.want, tt.ok)
		}
	}
}

// TestCheckKeepsStatus exercises check on a live response, whose request it
// names.
func TestCheckKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusConflict, "conflict")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/thing?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	err = check(resp)
	var responseErr *registry.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusConflict {
		t.Fatalf("check = %v, want a 409 response error", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "GET "+srv.URL+"/thing: 409 Conflict: conflict") || strings.Contains(msg, "secret") {
		t.Errorf("message = %q", msg)
	}
}
