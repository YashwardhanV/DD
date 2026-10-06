//go:build !windows

package volume

import "errors"

// InitProcess does nothing on non-Windows systems.
func InitProcess() {}

// List is not available outside Windows.
func List() ([]Volume, error) { return nil, errors.ErrUnsupported }

// ForPath is not available outside Windows.
func ForPath(path string) (Volume, error) { return Volume{}, errors.ErrUnsupported }

// ListLocal is not available outside Windows.
func ListLocal() ([]Volume, error) { return nil, errors.ErrUnsupported }
