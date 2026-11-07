//go:build !windows

package ntfs

import "errors"

// IsElevated is always false outside Windows.
func IsElevated() bool { return false }

// Open is not available outside Windows.
func Open(root string) (*Session, error) { return nil, errors.ErrUnsupported }

// WindowsLister is not available outside Windows; ListDir always fails.
type WindowsLister struct{}

// ListDir implements DirLister.
func (WindowsLister) ListDir(dir string) ([]DirRecord, error) { return nil, errors.ErrUnsupported }
