package fileio

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// writers are the two entry points, which differ only in the flush.
var writers = []struct {
	name  string
	write func(string, []byte, fs.FileMode) error
}{{"WriteFile", WriteFile}, {"WriteFileNoSync", WriteFileNoSync}}

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

// TestWriteFileNoSync: skipping the flush changes nothing a reader can see —
// the replacement is still atomic and still refuses to follow a symlink.
func TestWriteFileNoSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sha256:abc")
	for _, content := range []string{"first", "second"} {
		if err := WriteFileNoSync(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != content {
			t.Fatalf("file = %q, %v; want %q", got, err, content)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %v, want 0600", info.Mode().Perm())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, ".*")); len(files) != 0 {
		t.Errorf("temporary files leaked: %v", files)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoSync(link, []byte("overwrite"), 0o600); err == nil {
		t.Error("symlink accepted")
	}
}

// TestPreservesExistingMode: replacing a file keeps its mode even when perm is
// narrower, so a rewrite does not lock out the file's existing readers.
func TestPreservesExistingMode(t *testing.T) {
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "stats.json")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil { // independent of the umask
				t.Fatal(err)
			}
			if err := w.write(path, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
				t.Fatalf("file = %q, %v; want %q", got, err, "new")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Errorf("mode = %v, want 0644", info.Mode().Perm())
			}
			if files, _ := filepath.Glob(filepath.Join(dir, ".*")); len(files) != 0 {
				t.Errorf("temporary files leaked: %v", files)
			}
		})
	}
}

// TestUnwritablePaths: a path that cannot be written is an error, and it
// leaves the directory as it was, without a temporary file.
func TestUnwritablePaths(t *testing.T) {
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			dir := t.TempDir()
			file, directory := filepath.Join(dir, "file"), filepath.Join(dir, "directory")
			if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, tt := range []struct {
				name, path string
				is         error // nil: any error
			}{
				{"parent is a file", filepath.Join(file, "x"), syscall.ENOTDIR},
				{"missing parent", filepath.Join(dir, "missing", "x"), fs.ErrNotExist},
				{"directory target", directory, nil},
			} {
				err := w.write(tt.path, []byte("data"), 0o600)
				if err == nil || tt.is != nil && !errors.Is(err, tt.is) {
					t.Errorf("%s: error = %v, want %v", tt.name, err, tt.is)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if !slices.Equal(names, []string{"directory", "file"}) {
				t.Errorf("entries = %v, want [directory file]", names)
			}
			if entries, _ := os.ReadDir(directory); len(entries) != 0 {
				t.Errorf("directory target changed: %v", entries)
			}
			if got, _ := os.ReadFile(file); string(got) != "keep" {
				t.Errorf("file = %q, want %q", got, "keep")
			}
		})
	}
}
