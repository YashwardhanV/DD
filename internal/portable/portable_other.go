//go:build !windows

package portable

import "errors"

// List is not available outside Windows.
func List() ([]Device, error) { return nil, errors.ErrUnsupported }

// OpenDevice is not available outside Windows.
func OpenDevice(d Device) (Folder, error) { return nil, errors.ErrUnsupported }
