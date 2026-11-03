//go:build !windows

package copier

import "errors"

// FirstLCN is only available on Windows (NTFS).
func FirstLCN(path string) (int64, error) { return 0, errors.ErrUnsupported }
