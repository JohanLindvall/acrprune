package ghcr

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/JohanLindvall/crprune/internal/registry"

	"golang.org/x/sync/singleflight"
)

// tokenCache holds the registry's bearer tokens by scope, along with the token
// endpoint, learned from the registry's first challenge.
//
// The registry API does not take the GitHub token directly: following the
// distribution spec's token flow, a request without a valid bearer token is
// answered with a challenge naming a token endpoint, where the GitHub token is
// exchanged for a registry token scoped to one repository.
type tokenCache struct {
	mu      sync.Mutex
	byScope map[string]string
	realm   string
	service string
	flight  singleflight.Group
}

// registryToken returns a token for scope: a cached one, or a fresh one when
// the token endpoint is known. It returns "" before the first challenge, so
// that the first request goes out unauthenticated to learn the endpoint.
func (b *Backend) registryToken(ctx context.Context, scope string) (string, error) {
	b.tokens.mu.Lock()
	token, realm := b.tokens.byScope[scope], b.tokens.realm
	b.tokens.mu.Unlock()
	if token != "" || realm == "" {
		return token, nil
	}
	return b.fetchRegistryToken(ctx, scope, "")
}

// renewRegistryToken replaces a token the registry rejected with the given
// challenge, or supplies the missing one. A token that a concurrent request
// already renewed is reused.
func (b *Backend) renewRegistryToken(ctx context.Context, scope, rejected, challenge string) (string, error) {
	realm, service, err := b.parseChallenge(challenge)
	if err != nil {
		return "", err
	}
	b.tokens.mu.Lock()
	b.tokens.realm, b.tokens.service = realm, service
	current := b.tokens.byScope[scope]
	b.tokens.mu.Unlock()
	if current != "" && current != rejected {
		return current, nil
	}
	return b.fetchRegistryToken(ctx, scope, rejected)
}

// fetchRegistryToken exchanges the GitHub token for a registry token for
// scope. Concurrent requests for one scope share a single exchange.
func (b *Backend) fetchRegistryToken(ctx context.Context, scope, rejected string) (string, error) {
	result := b.tokens.flight.DoChan(scope, func() (any, error) {
		b.tokens.mu.Lock()
		realm, service := b.tokens.realm, b.tokens.service
		current := b.tokens.byScope[scope]
		b.tokens.mu.Unlock()
		// A request may have renewed the token between the caller's lookup
		// and entering this flight. Do not exchange it again in that gap.
		if current != "" && current != rejected {
			return current, nil
		}

		u, err := url.Parse(realm)
		if err != nil {
			return "", err
		}
		query := u.Query()
		query.Set("scope", scope)
		if service != "" {
			query.Set("service", service)
		}
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", err
		}
		req.SetBasicAuth(b.username, b.token)
		resp, err := b.send(req)
		if err != nil {
			return "", err
		}
		if err := check(resp); err != nil {
			return "", fmt.Errorf("failed to get a registry token: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body); err != nil {
			return "", fmt.Errorf("failed to decode the registry token: %w", err)
		}
		token := cmp.Or(body.Token, body.AccessToken)
		if token == "" {
			return "", errors.New("the registry's token response carries no token")
		}
		b.tokens.mu.Lock()
		b.tokens.byScope[scope] = token
		b.tokens.mu.Unlock()
		return token, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case token := <-result:
		if token.Err != nil {
			return "", token.Err
		}
		return token.Val.(string), nil
	}
}

// parseChallenge extracts the token endpoint from a WWW-Authenticate header
// such as `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="…"`.
// The GitHub token is sent to the realm, so it must be on the registry's own
// origin.
func (b *Backend) parseChallenge(header string) (realm, service string, err error) {
	scheme, params, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", "", fmt.Errorf("unsupported registry authentication challenge %q", header)
	}
	values := parseParams(params)
	realm = values["realm"]
	u, err := url.Parse(realm)
	if err != nil || u.User != nil || u.Fragment != "" || !registry.SameOrigin(u, b.registryURL) {
		return "", "", fmt.Errorf("refusing to authenticate at %q, away from %s", realm, b.registryURL.Host)
	}
	return realm, values["service"], nil
}

// parseParams parses the comma-separated key=value or key="value" parameters
// of an authentication challenge. Quoted values may contain commas, and a
// backslash escapes the character after it.
func parseParams(s string) map[string]string {
	values := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t,")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			return values
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(rest, `"`) {
			var value string
			value, s, _ = strings.Cut(rest, ",")
			values[key] = strings.TrimSpace(value)
			continue
		}
		var value strings.Builder
		i := 1
		for ; i < len(rest) && rest[i] != '"'; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
			}
			value.WriteByte(rest[i])
		}
		values[key] = value.String()
		s = rest[min(i+1, len(rest)):]
	}
}
