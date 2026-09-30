package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/JohanLindvall/acrprune/internal/fileio"
)

// digestPattern matches an `algorithm:hex` OCI digest. Digests become file
// names, so anything else is refused rather than allowed to name a path
// outside the cache directory.
var digestPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// Cache stores downloaded manifest documents on disk, keyed by digest.
// A nil *Cache is valid and disables caching.
type Cache struct {
	dir string
}

// NewCache returns a cache rooted at dir, creating the directory, or nil when
// dir is empty. An unusable dir — a file, or a path that cannot be created —
// is an error, so that a mistyped --cache fails fast instead of silently
// caching nothing.
func NewCache(dir string) (*Cache, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("unusable cache directory: %w", err)
	}
	return &Cache{dir: dir}, nil
}

// path returns the file backing digest, or "" when the cache is disabled or
// the digest is not a well-formed, safe file name.
func (c *Cache) path(digest string) string {
	if c == nil || !digestPattern.MatchString(digest) {
		return ""
	}
	return filepath.Join(c.dir, digest)
}

// Get returns the cached document for digest, or nil.
func (c *Cache) Get(digest string) []byte {
	path := c.path(digest)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := ReadDocument(f)
	if err != nil {
		return nil
	}
	return data
}

// Put stores a document. Documents the cache does not take — an unsafe digest,
// an oversized document, or any document when caching is disabled — are
// skipped without error. The write is atomic but not flushed to disk: every
// read is verified against the digest, so a document torn by a crash is
// merely downloaded again.
func (c *Cache) Put(digest string, data []byte) error {
	path := c.path(digest)
	if path == "" || len(data) > MaxDocumentSize {
		return nil
	}
	return fileio.WriteFileNoSync(path, data, 0o600)
}

// Remove drops a document from the cache.
func (c *Cache) Remove(digest string) {
	if path := c.path(digest); path != "" {
		_ = os.Remove(path)
	}
}
