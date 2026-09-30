package fileio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplacementAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFile(path, []byte("first snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = previous.Close() }()
	if err := WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := make([]byte, 14)
	if _, err := previous.Read(old); err != nil {
		t.Fatal(err)
	}
	if string(old) != "first snapshot" {
		t.Fatalf("old inode was overwritten: %q", old)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "short" {
		t.Fatalf("new snapshot = %q, %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions widened: %v", info.Mode())
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*"))
	if len(files) != 0 {
		t.Fatalf("temporary files leaked: %v", files)
	}
}

func TestDoesNotFollowSymlinkOnWrite(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("overwrite"), 0o600); err == nil {
		t.Fatal("symlink accepted")
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Fatal("symlink target overwritten")
	}
}
