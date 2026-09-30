// Package fileio writes complete file snapshots without exposing partial data.
package fileio

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data. A failure leaves the previous
// file intact. Existing regular-file permissions are preserved; new files use
// perm. The temporary file is private until its contents are complete.
func WriteFile(path string, data []byte, perm fs.FileMode) (err error) {
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot replace non-regular file %s", path)
		}
		perm = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
