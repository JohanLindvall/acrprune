package registry

import (
	"context"
	"errors"
	"net/http"
)

// Backend is the registry-specific half of a Registry: the primitive
// operations of one registry's API. Registry adds parallelism, caching, digest
// verification and logging on top.
//
// Every failed request should surface as, or wrap, a *ResponseError so that
// callers can tell a missing or forbidden resource from other failures.
type Backend interface {
	// ListRepositories returns the names of all repositories.
	ListRepositories(ctx context.Context) ([]string, error)
	// ListManifests calls fn, sequentially, with the attributes of every
	// manifest in the repository as the listing pages arrive, so downloads can
	// start before the listing is complete. An error from fn stops the
	// listing.
	ListManifests(ctx context.Context, repository string, fn func(Attributes) error) error
	// GetManifest downloads a manifest document. The caller verifies it
	// against the digest.
	GetManifest(ctx context.Context, repository, digest string) ([]byte, error)
	// DeleteManifest deletes a manifest along with the tags pointing at it.
	// Deleting a manifest that is already gone succeeds.
	DeleteManifest(ctx context.Context, m *Manifest) error
	// DeleteRepository deletes a repository and everything in it.
	DeleteRepository(ctx context.Context, repository string) error
}

// Unlocker is implemented by backends whose registry can lock manifests and
// tags against deletion (ACR image locks).
type Unlocker interface {
	// LockedTags returns the names of the repository's tags that are locked.
	LockedTags(ctx context.Context, repository string) (map[string]bool, error)
	// UnlockManifest re-enables delete and write on a manifest.
	UnlockManifest(ctx context.Context, m *Manifest) error
	// UnlockTag re-enables delete and write on a tag.
	UnlockTag(ctx context.Context, repository, tag string) error
}

// BlobGetter is implemented by backends whose listings report no platform
// (GHCR). Registry then reads the platform of an image from its config blob.
type BlobGetter interface {
	// GetBlob downloads a small blob such as an image config. The caller
	// verifies it against the digest.
	GetBlob(ctx context.Context, repository, digest string) ([]byte, error)
}

// LastTagProtector is implemented by backends whose registry refuses to
// delete the last tagged manifest of a repository it is not deleting outright
// (GHCR: "You cannot delete the last tagged version of a package").
type LastTagProtector interface {
	ProtectsLastTag() bool
}

// ResponseError is an error response from a registry API.
type ResponseError struct {
	StatusCode int
	// Err describes the failure; it may be the backend's own error type.
	Err error
}

func (e *ResponseError) Error() string {
	if e.Err == nil {
		return http.StatusText(e.StatusCode)
	}
	return e.Err.Error()
}

func (e *ResponseError) Unwrap() error {
	return e.Err
}

// IsPermissionError reports whether err is a 401/403 response, i.e. the
// caller lacks permission on the resource (typical on ABAC registries scoped
// to a subset of repositories).
func IsPermissionError(err error) bool {
	return hasStatus(err, http.StatusUnauthorized) || hasStatus(err, http.StatusForbidden)
}

// IsNotFound reports whether err is a 404 response.
func IsNotFound(err error) bool {
	return hasStatus(err, http.StatusNotFound)
}

// hasStatus reports whether err is, or wraps, a response error carrying the
// given status code.
func hasStatus(err error, status int) bool {
	var responseErr *ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}
