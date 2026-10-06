//go:build !windows

package ntfs

import "errors"

// IsElevated is always false outside Windows.
func IsElevated() bool { return false }

// Open is not available outside Windows.
func Open(root string) (*Session, error) { return nil, errors.ErrUnsupported }
