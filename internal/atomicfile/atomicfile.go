// Package atomicfile writes a download under a temporary name next to its destination,
// and renames it into place once it is complete: an interrupted transfer never leaves a
// partial file under the final name.
package atomicfile

import (
	"os"
	"path/filepath"
)

// File is a temporary file that Commit renames over its destination.
type File struct {
	*os.File
	path      string
	committed bool
}

// Create opens a temporary file in the directory of path, named after pattern (see
// os.CreateTemp). It is readable by its owner only.
func Create(path, pattern string) (*File, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return nil, err
	}
	return &File{File: tmp, path: path}, nil
}

// Commit closes the file and renames it over its destination. A destination that
// already exists is replaced and keeps its mode; a new one stays readable by its owner
// only.
func (f *File) Commit() error {
	if st, err := os.Stat(f.path); err == nil {
		if err := f.Chmod(st.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), f.path); err != nil {
		return err
	}
	f.committed = true
	return nil
}

// Discard closes and removes the temporary file, unless Commit renamed it. It is meant
// to be deferred right after Create.
func (f *File) Discard() {
	if f.committed {
		return
	}
	f.Close()
	os.Remove(f.Name())
}
