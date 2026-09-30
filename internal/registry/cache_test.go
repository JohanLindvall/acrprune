package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func newCache(t *testing.T, dir string) *Cache {
	t.Helper()
	c, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub") // NewCache creates it
	c := newCache(t, dir)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir should exist and be private: %v, %v", info, err)
	}
	if c.Get("sha256:x") != nil {
		t.Error("empty cache should miss")
	}
	if err := c.Put("sha256:x", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if string(c.Get("sha256:x")) != "data" {
		t.Error("cache should hit after Put")
	}
	if info, err := os.Stat(filepath.Join(dir, "sha256:x")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cached file should be private: %v, %v", info, err)
	}
	c.Remove("sha256:x")
	if c.Get("sha256:x") != nil {
		t.Error("cache should miss after Remove")
	}
}

func TestNilCache(t *testing.T) {
	c := newCache(t, "")
	if c != nil {
		t.Fatal("empty dir should yield nil cache")
	}
	// All operations must be nil-safe no-ops.
	if err := c.Put("d", []byte("x")); err != nil {
		t.Errorf("nil cache Put = %v", err)
	}
	if c.Get("d") != nil {
		t.Error("nil cache should always miss")
	}
	c.Remove("d")
}

// TestNewCacheRejectsUnusableDir: a --cache naming a file, or a directory
// below one, fails up front instead of silently never caching.
func TestNewCacheRejectsUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{file, filepath.Join(file, "myreg.azurecr.io")} {
		if c, err := NewCache(dir); err == nil {
			t.Errorf("NewCache(%s) = %v, want an error", dir, c)
		}
	}
}

// TestCachePutFailure: a write that fails is reported, so the registry can
// warn about it; skipped documents are not failures.
func TestCachePutFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c := newCache(t, dir)
	if err := c.Put("sha256:x", make([]byte, MaxDocumentSize+1)); err != nil {
		t.Errorf("an oversized document should be skipped, got %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("sha256:x", []byte("data")); err == nil {
		t.Error("a failed write should be reported")
	}
}

// TestCacheRejectsUnsafeDigests keeps digests from naming files outside the
// cache directory: they are used verbatim as the file name.
func TestCacheRejectsUnsafeDigests(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	c := newCache(t, dir)

	unsafe := []string{
		"",
		"../escaped",
		"sha256:../../escaped",
		"sha256:ab/cd",
		"nocolon",
		"/absolute",
	}
	for _, digest := range unsafe {
		if err := c.Put(digest, []byte("payload")); err != nil {
			t.Errorf("digest %q should be skipped, got %v", digest, err)
		}
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
