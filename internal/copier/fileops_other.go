//go:build !windows

package copier

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

// preallocate is a no-op off Windows (DriveDex only ships for Windows; the
// tests run everywhere).
func preallocate(*os.File, int64) {}

// setModTime returns false: the caller sets the time by name after closing.
func setModTime(*os.File, time.Time) (bool, error) { return false, nil }

// openSourceSingle off Windows is v1's two steps (Lstat, then Open); it
// returns the size and time Lstat saw, so the logic above it is the same.
func openSourceSingle(path string) (*os.File, int64, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, 0, errSourceGone
		}
		return nil, 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, 0, errNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	return f, info.Size(), info.ModTime().Unix(), nil
}

// openUnbuffered / createUnbuffered: unbuffered I/O is a Windows feature;
// elsewhere the caller falls back to normal I/O.
func openUnbuffered(path string) (*os.File, error)   { return nil, errors.ErrUnsupported }
func createUnbuffered(path string) (*os.File, error) { return nil, errors.ErrUnsupported }

// setEOF sets the file's length.
func setEOF(f *os.File, size int64) error { return f.Truncate(size) }
