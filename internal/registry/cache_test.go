package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub") // exercise MkdirAll in Put
	c := NewCache(dir)
	if c.Get("sha256:x") != nil {
		t.Error("empty cache should miss")
	}
	c.Put("sha256:x", []byte("data"))
	if string(c.Get("sha256:x")) != "data" {
		t.Error("cache should hit after Put")
	}
	c.Remove("sha256:x")
	if c.Get("sha256:x") != nil {
		t.Error("cache should miss after Remove")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache dir should exist: %v", err)
	}
}

func TestNilCache(t *testing.T) {
	c := NewCache("")
	if c != nil {
		t.Fatal("empty dir should yield nil cache")
	}
	// All operations must be nil-safe no-ops.
	c.Put("d", []byte("x"))
	if c.Get("d") != nil {
		t.Error("nil cache should always miss")
	}
	c.Remove("d")
}

// TestCacheRejectsUnsafeDigests keeps digests from naming files outside the
// cache directory: they are used verbatim as the file name.
func TestCacheRejectsUnsafeDigests(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	c := NewCache(dir)

	unsafe := []string{
		"",
		"../escaped",
		"sha256:../../escaped",
		"sha256:ab/cd",
		"nocolon",
		"/absolute",
	}
	for _, digest := range unsafe {
		c.Put(digest, []byte("payload"))
		if got := c.Get(digest); got != nil {
			t.Errorf("digest %q should not be cacheable, got %q", digest, got)
		}
		c.Remove(digest)
	}

	// Nothing may have been created outside the cache directory.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cache" {
			t.Errorf("unexpected entry written outside the cache dir: %s", e.Name())
		}
	}
}
