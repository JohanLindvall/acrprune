// Package ghcr implements registry.Backend for GitHub Container Registry.
//
// ghcr.io's registry API serves manifest and blob content but can neither list
// a repository's untagged manifests nor delete anything, so listing and
// deletion go through the GitHub REST API's packages endpoints instead: each
// repository is a container package of the owning user or organization, and
// each manifest one of the package's versions.
package ghcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"

	"github.com/JohanLindvall/acrprune/internal/imageref"
	"github.com/JohanLindvall/acrprune/internal/registry"
)

var (
	_ registry.Backend          = (*Backend)(nil)
	_ registry.BlobGetter       = (*Backend)(nil)
	_ registry.LastTagProtector = (*Backend)(nil)
)

const (
	// DefaultAPIURL is the GitHub REST API.
	DefaultAPIURL = "https://api.github.com"
	// DefaultRegistryURL is GitHub Container Registry's registry API.
	DefaultRegistryURL = "https://" + registry.GHCRHost

	// maxPageSize is the most items the REST API returns per page.
	maxPageSize = 100
	// defaultUsername accompanies the token at the registry's token
	// endpoint, which authenticates by the token alone.
	defaultUsername = "acrprune"
	userAgent       = "acrprune"
	apiVersion      = "2022-11-28"
)

// Options configures a Backend.
type Options struct {
	// Owner is the user or organization owning the packages.
	Owner string
	// Token is a GitHub token with the read:packages scope, and
	// delete:packages to delete.
	Token string
	// Username accompanies Token at the registry's token endpoint. GHCR does
	// not check it; it defaults to a placeholder.
	Username string
	// PageSize is the number of items per listing request, capped at the
	// REST API's maximum of 100.
	PageSize int
	// Logger receives warnings about retried requests.
	Logger *slog.Logger
	// APIURL and RegistryURL default to DefaultAPIURL and
	// DefaultRegistryURL.
	APIURL, RegistryURL string
	// HTTPClient defaults to a client with a two-minute request timeout.
	HTTPClient *http.Client
}

// Backend speaks to GHCR on behalf of one owner.
type Backend struct {
	owner       string
	token       string
	username    string
	pageSize    int
	apiURL      *url.URL
	registryURL *url.URL
	// packages is the REST URL of the owner's packages, which differs
	// between users and organizations.
	packages string
	client   *http.Client
	logger   *slog.Logger
	tokens   tokenCache

	// maxAttempts bounds the attempts at a request failing transiently, and
	// maxThrottle how long one request waits out rate limiting.
	maxAttempts int
	maxThrottle time.Duration
	// sleep waits before a retry; tests replace it.
	sleep func(context.Context, time.Duration) error
}

// New returns a backend for the owner's container packages. It looks the owner
// up to learn whether it is a user or an organization, whose packages live
// under different REST paths, and so also checks the token.
func New(ctx context.Context, opts Options) (*Backend, error) {
	if opts.Token == "" {
		return nil, errors.New("a GitHub token is required")
	}
	if !imageref.ValidOwner(opts.Owner) {
		return nil, fmt.Errorf("invalid GHCR owner %q", opts.Owner)
	}
	if opts.PageSize < 1 {
		return nil, fmt.Errorf("page size must be at least 1, got %d", opts.PageSize)
	}
	apiURL, err := baseURL(opts.APIURL, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	registryURL, err := baseURL(opts.RegistryURL, DefaultRegistryURL)
	if err != nil {
		return nil, err
	}
	b := &Backend{
		owner:       opts.Owner,
		token:       opts.Token,
		username:    opts.Username,
		pageSize:    min(opts.PageSize, maxPageSize),
		apiURL:      apiURL,
		registryURL: registryURL,
		client:      opts.HTTPClient,
		logger:      opts.Logger,
		tokens:      tokenCache{byScope: map[string]string{}},
		maxAttempts: 6,
		maxThrottle: 2 * time.Hour,
		sleep:       sleep,
	}
	if b.username == "" {
		b.username = defaultUsername
	}
	if b.client == nil {
		b.client = &http.Client{Timeout: 2 * time.Minute}
	}
	if b.logger == nil {
		b.logger = slog.New(slog.DiscardHandler)
	}

	namespace, err := b.namespace(ctx)
	if err != nil {
		return nil, err
	}
	b.packages = b.apiURL.String() + "/" + namespace + "/" + url.PathEscape(b.owner) + "/packages"
	return b, nil
}

// baseURL parses a service URL, falling back to def when it is empty.
func baseURL(s, def string) (*url.URL, error) {
	if s == "" {
		s = def
	}
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid service URL %q", s)
	}
	return u, nil
}

// namespace returns the REST path segment of the owner's kind of account:
// "orgs" for an organization, "users" otherwise.
func (b *Backend) namespace(ctx context.Context) (string, error) {
	resp, err := b.api(ctx, http.MethodGet, b.apiURL.String()+"/users/"+url.PathEscape(b.owner))
	if registry.IsNotFound(err) {
		return "", fmt.Errorf("no GitHub user or organization named %q", b.owner)
	}
	if err != nil {
		return "", fmt.Errorf("failed to look up GitHub account %s: %w", b.owner, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var account struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil {
		return "", fmt.Errorf("failed to decode GitHub account %s: %w", b.owner, err)
	}
	if account.Type == "Organization" {
		return "orgs", nil
	}
	return "users", nil
}

// ListRepositories returns the names of the owner's container packages.
func (b *Backend) ListRepositories(ctx context.Context) ([]string, error) {
	names := []string{}
	err := b.paginate(ctx, b.packages+"?package_type=container&per_page="+strconv.Itoa(b.pageSize), func(body io.Reader) error {
		var page []struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return fmt.Errorf("failed to decode packages: %w", err)
		}
		for _, p := range page {
			names = append(names, p.Name)
		}
		return nil
	})
	if registry.IsPermissionError(err) {
		return nil, fmt.Errorf("failed to list the packages of %s (listing needs a token with the read:packages scope): %w", b.owner, err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list the packages of %s: %w", b.owner, err)
	}
	return names, nil
}

// packageVersion is the part of a package version the pruner needs.
type packageVersion struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // the manifest digest
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Metadata  struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

// attributes converts a package version. The listing reports no platform;
// Registry resolves it when a rule needs it.
func (v packageVersion) attributes() registry.Attributes {
	updated := v.UpdatedAt
	if updated.IsZero() {
		updated = v.CreatedAt
	}
	return registry.Attributes{
		Digest:      v.Name,
		Tags:        v.Metadata.Container.Tags,
		LastUpdated: updated,
		ID:          strconv.FormatInt(v.ID, 10),
	}
}

// ListManifests calls fn with the attributes of every version of the
// repository's package.
func (b *Backend) ListManifests(ctx context.Context, repository string, fn func(registry.Attributes) error) error {
	if !imageref.ValidRepository(repository) {
		return fmt.Errorf("invalid repository name %q", repository)
	}
	return b.paginate(ctx, b.packageURL(repository)+"/versions?per_page="+strconv.Itoa(b.pageSize), func(body io.Reader) error {
		var page []packageVersion
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return fmt.Errorf("failed to decode package versions: %w", err)
		}
		for _, v := range page {
			if err := fn(v.attributes()); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetManifest downloads a manifest document from the registry API.
func (b *Backend) GetManifest(ctx context.Context, repository, digest string) ([]byte, error) {
	return b.registryGet(ctx, repository, "manifests", digest, registry.ManifestMediaTypes)
}

// GetBlob downloads a small blob, such as an image config, from the registry
// API.
func (b *Backend) GetBlob(ctx context.Context, repository, digest string) ([]byte, error) {
	return b.registryGet(ctx, repository, "blobs", digest, "")
}

// DeleteManifest deletes the package version holding the manifest, and with
// it the tags pointing at the manifest.
func (b *Backend) DeleteManifest(ctx context.Context, m *registry.Manifest) error {
	if !imageref.ValidRepository(m.Repository) {
		return fmt.Errorf("invalid repository name %q", m.Repository)
	}
	// The id goes into the URL path; it must be the number the listing gave.
	if _, err := strconv.ParseUint(m.ID, 10, 64); err != nil {
		return fmt.Errorf("manifest %s has no package version id", m.Ref())
	}
	return b.delete(ctx, b.packageURL(m.Repository)+"/versions/"+m.ID)
}

// DeleteRepository deletes the repository's package with all its versions.
func (b *Backend) DeleteRepository(ctx context.Context, repository string) error {
	if !imageref.ValidRepository(repository) {
		return fmt.Errorf("invalid repository name %q", repository)
	}
	return b.delete(ctx, b.packageURL(repository))
}

// ProtectsLastTag reports that GHCR refuses to delete a package's last tagged
// version: "You must delete the package instead".
func (b *Backend) ProtectsLastTag() bool {
	return true
}

// packageURL returns the REST URL of the repository's package. The name is a
// single path segment there, with any slash escaped.
func (b *Backend) packageURL(repository string) string {
	return b.packages + "/container/" + url.PathEscape(repository)
}

// delete issues a DELETE, treating a resource that is already gone as
// deleted, as ACR does. That is unusual right after listing it, so it is
// logged.
func (b *Backend) delete(ctx context.Context, u string) error {
	resp, err := b.api(ctx, http.MethodDelete, u)
	if registry.IsNotFound(err) {
		b.logger.Warn("Nothing to delete; treating as already deleted", "err", err)
		return nil
	}
	if err != nil {
		return err
	}
	return discard(resp)
}

// api sends a REST API request, returning the response to a successful one.
func (b *Backend) api(ctx context.Context, method, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := b.send(req)
	if err != nil {
		return nil, err
	}
	if err := check(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// paginate GETs a listing and every further page its Link headers point to,
// handing each page's body to fn.
func (b *Backend) paginate(ctx context.Context, u string, fn func(io.Reader) error) error {
	seen := map[string]struct{}{}
	for u != "" {
		if _, duplicate := seen[u]; duplicate {
			return fmt.Errorf("pagination repeated a page: %s", redactURL(u))
		}
		seen[u] = struct{}{}
		resp, err := b.api(ctx, http.MethodGet, u)
		if err != nil {
			return err
		}
		err = fn(io.LimitReader(resp.Body, registry.MaxDocumentSize))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if u, err = b.nextPage(resp.Header.Get("Link")); err != nil {
			return err
		}
	}
	return nil
}

// nextPage returns the rel="next" target of a Link header, or "" on the last
// page. Requests to it carry the token, so it must stay on the API's origin.
func (b *Backend) nextPage(link string) (string, error) {
	for _, entry := range strings.Split(link, ",") {
		target, params, _ := strings.Cut(entry, ";")
		next := false
		for _, param := range strings.Split(params, ";") {
			param = strings.TrimSpace(param)
			next = next || strings.EqualFold(param, `rel="next"`) || strings.EqualFold(param, "rel=next")
		}
		if !next {
			continue
		}
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		u, err := url.Parse(target)
		if err != nil || u.User != nil || u.Fragment != "" || !sameOrigin(u, b.apiURL) {
			return "", fmt.Errorf("refusing to follow the next page link %q away from %s", target, b.apiURL.Host)
		}
		return target, nil
	}
	return "", nil
}

// registryGet downloads a manifest or blob from the registry API,
// authenticating with a bearer token for the repository's pull scope.
func (b *Backend) registryGet(ctx context.Context, repository, kind, digest, accept string) ([]byte, error) {
	if !imageref.ValidRepository(repository) {
		return nil, fmt.Errorf("invalid repository name %q", repository)
	}
	if _, err := godigest.Parse(digest); err != nil {
		return nil, fmt.Errorf("invalid digest %q: %w", digest, err)
	}
	path := b.owner + "/" + repository
	u := b.registryURL.String() + "/v2/" + path + "/" + kind + "/" + digest
	scope := "repository:" + path + ":pull"

	token, err := b.registryToken(ctx, scope)
	if err != nil {
		return nil, err
	}
	for renewed := false; ; renewed = true {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := b.send(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !renewed {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = discard(resp)
			if token, err = b.renewRegistryToken(ctx, scope, token, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if err := check(resp); err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		return registry.ReadDocument(resp.Body)
	}
}
