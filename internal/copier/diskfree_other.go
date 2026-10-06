//go:build !windows

package copier

import (
	"errors"
	"syscall"
)

// diskFree returns the free bytes available to the current user on the file
// system holding path.
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// fileSystemOf is not available outside Windows (unknown).
func fileSystemOf(path string) (string, error) { return "", errors.ErrUnsupported }
