package registry

import (
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

// NewCache returns a cache rooted at dir, or nil when dir is empty.
func NewCache(dir string) *Cache {
	if dir == "" {
		return nil
	}
	return &Cache{dir: dir}
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

// Put stores a document; cache write failures are ignored.
func (c *Cache) Put(digest string, data []byte) {
	path := c.path(digest)
	if path == "" || len(data) > MaxDocumentSize {
		return
	}
	if err := os.MkdirAll(c.dir, 0o700); err == nil {
		_ = fileio.WriteFile(path, data, 0o600)
	}
}

// Remove drops a document from the cache.
func (c *Cache) Remove(digest string) {
	if path := c.path(digest); path != "" {
		_ = os.Remove(path)
	}
}
