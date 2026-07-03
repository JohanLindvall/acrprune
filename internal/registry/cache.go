package registry

import (
	"os"
	"path/filepath"
)

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

// Get returns the cached document for digest, or nil.
func (c *Cache) Get(digest string) []byte {
	if c == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(c.dir, digest))
	if err != nil {
		return nil
	}
	return data
}

// Put stores a document; cache write failures are ignored.
func (c *Cache) Put(digest string, data []byte) {
	if c == nil {
		return
	}
	if err := os.MkdirAll(c.dir, 0755); err == nil {
		_ = os.WriteFile(filepath.Join(c.dir, digest), data, 0644)
	}
}

// Remove drops a document from the cache.
func (c *Cache) Remove(digest string) {
	if c == nil {
		return
	}
	os.Remove(filepath.Join(c.dir, digest))
}
