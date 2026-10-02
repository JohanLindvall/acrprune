package acr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/JohanLindvall/crprune/internal/registry"
)

// CredentialOptions returns the options for the DefaultAzureCredential that
// authenticates to the registry at the login server host. Unless
// AZURE_AUTHORITY_HOST names the Microsoft Entra ID authority explicitly, they
// select the host's cloud (see Cloud): service principals and workload
// identities of Azure China and Azure Government tenants cannot sign in at the
// public cloud's authority.
func CredentialOptions(host string) *azidentity.DefaultAzureCredentialOptions {
	options := &azidentity.DefaultAzureCredentialOptions{}
	if os.Getenv("AZURE_AUTHORITY_HOST") == "" {
		options.Cloud = Cloud(host)
	}
	return options
}

// Cloud returns the Azure cloud a registry login server belongs to, by its
// suffix: Azure China for .azurecr.cn and Azure Government for .azurecr.us.
// For any other host it returns the zero configuration, which leaves the
// choice to the credential: the public cloud, or AZURE_AUTHORITY_HOST. The
// registry client itself needs no cloud: every cloud's ACR shares the token
// audience the SDK defaults to.
func Cloud(host string) cloud.Configuration {
	switch {
	case strings.HasSuffix(host, ".azurecr.cn"):
		return cloud.AzureChina
	case strings.HasSuffix(host, ".azurecr.us"):
		return cloud.AzureGovernment
	}
	return cloud.Configuration{}
}

// documentTransport bounds every response before the SDK's body download
// policy reads it into memory, including error and token responses. Reading
// here also puts truncated streaming responses inside the retry loop.
type documentTransport struct{ policy.Transporter }

func (t documentTransport) Do(req *http.Request) (*http.Response, error) {
	resp, err := t.Transporter.Do(req)
	if err != nil {
		return resp, err
	}
	if err := registry.BufferResponse(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// gates lets the first request of each action on a repository go alone. The
// SDK client answers its 401 challenge by obtaining an access token for that
// repository and action and caching it; the requests queued behind it then
// reuse the token instead of each obtaining one.
type gates struct {
	mu   sync.Mutex
	open map[string]chan struct{} // closed once the first request completed
}

// do runs fn, a request of the action on the repository. Unless it is the
// first such request, it first waits for the first one to complete, or for ctx
// to end.
func (g *gates) do(ctx context.Context, action, repository string, fn func() error) error {
	key := action + " " + repository
	g.mu.Lock()
	if g.open == nil {
		g.open = map[string]chan struct{}{}
	}
	gate, started := g.open[key]
	if !started {
		gate = make(chan struct{})
		g.open[key] = gate
	}
	g.mu.Unlock()
	if !started {
		defer close(gate)
		return fn()
	}
	select {
	case <-gate:
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryPolicy retries throttled and transiently failing requests, in place of
// the SDK's retries, which are silent and give up on throttling within
// seconds. It waits as long as the registry asks (Retry-After), or else backs
// off exponentially from delay, in either case up to maxDelay, and logs every
// retry as a warning whose delay the progress display counts down.
type retryPolicy struct {
	maxRetries      int
	delay, maxDelay time.Duration
	logger          *slog.Logger
	// sleep waits before a retry; tests replace it.
	sleep func(context.Context, time.Duration) error
}

// defaultRetry rides out throttling of a quarter of an hour or so per request.
var defaultRetry = retryPolicy{maxRetries: 10, delay: 2 * time.Second, maxDelay: 3 * time.Minute, sleep: registry.Sleep}

// retryStatuses are the responses worth retrying: timeouts, throttling and
// server-side failures.
var retryStatuses = []int{
	http.StatusRequestTimeout,
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

// Do implements policy.Policy.
func (p *retryPolicy) Do(req *policy.Request) (*http.Response, error) {
	ctx := req.Raw().Context()
	for retry := 0; ; retry++ {
		if err := req.RewindBody(); err != nil {
			return nil, err
		}
		resp, err := req.Clone(ctx).Next()
		// Validate before the generated response handlers run. The SDK
		// assumes well-formed pagination links and non-nil token fields.
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			switch path := req.Raw().URL.Path; {
			case strings.HasPrefix(path, "/acr/v1/"):
				err = normalizePagination(resp, req.Raw().URL)
			case path == "/oauth2/token" || path == "/oauth2/exchange":
				err = validateTokenResponse(resp, path)
			}
		}
		err = registry.RedactError(err)
		if ctx.Err() != nil {
			return resp, err
		}
		var reason string
		var delay time.Duration
		switch {
		case err != nil:
			if !registry.Transient(err) {
				return nil, err
			}
			reason = err.Error()
		case !slices.Contains(retryStatuses, resp.StatusCode):
			return resp, nil
		default:
			reason = resp.Status
			delay = retryAfter(resp, time.Now())
		}
		if retry >= p.maxRetries {
			return resp, err
		}
		if delay <= 0 {
			delay = p.backoff(retry)
		}
		delay = min(delay, p.maxDelay)
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
		registry.LogRetry(p.logger, req.Raw().Method, req.Raw().URL, reason, delay, time.Now())
		if err := p.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// The SDK dereferences these token fields without checking whether a 200
// response included them. Reject malformed responses without logging tokens.
func validateTokenResponse(resp *http.Response, path string) error {
	// The SDK's generated decoder matches exact JSON keys, unlike a Go
	// struct decoder, which also accepts differently capitalized names.
	var fields map[string]json.RawMessage
	if err := runtime.UnmarshalAsJSON(resp, &fields); err != nil {
		return errors.New("invalid registry token response")
	}
	key := "access_token"
	if path == "/oauth2/exchange" {
		key = "refresh_token"
	}
	var value string
	if err := json.Unmarshal(fields[key], &value); err != nil || strings.TrimSpace(value) == "" {
		return errors.New("registry token response carries no token")
	}
	return nil
}

// The SDK assumes Link starts with '<' and contains '>', and panics when it
// does not. It also appends the target to its endpoint even for absolute URLs.
// Validate before its response handler runs and give it only a relative URI.
func normalizePagination(resp *http.Response, origin *url.URL) error {
	next, err := registry.NextPage(resp.Header.Values("Link"), resp.Request.URL)
	if err != nil {
		return err
	}
	resp.Header.Del("Link")
	if next != "" {
		u, _ := url.Parse(next)
		if !registry.SameOrigin(u, origin) {
			return fmt.Errorf("refusing to follow the next page link away from %s", origin.Host)
		}
		resp.Header.Set("Link", "<"+u.RequestURI()+">; rel=\"next\"")
	}
	return nil
}

// backoff returns the delay before the given retry, counted from zero, of a
// request the registry did not say when to retry: delay, doubling with every
// retry up to maxDelay.
func (p *retryPolicy) backoff(retry int) time.Duration {
	delay := p.delay
	for range retry {
		if delay >= p.maxDelay/2 {
			return p.maxDelay
		}
		delay *= 2
	}
	return min(delay, p.maxDelay)
}

// retryAfter returns how long a response asks to wait before retrying, or zero
// when it does not say.
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	for _, header := range []string{"Retry-After-Ms", "X-Ms-Retry-After-Ms"} {
		if ms, err := strconv.ParseInt(resp.Header.Get(header), 10, 64); err == nil && ms > 0 {
			return time.Duration(min(ms, int64(time.Hour/time.Millisecond))) * time.Millisecond
		}
	}
	value := resp.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, int64(time.Hour/time.Second))) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.Sub(now)
	}
	return 0
}
