//go:build !windows

package volume

import "errors"

// BusType is not available outside Windows.
func BusType(root string) (BusInfo, error) { return BusInfo{}, errors.ErrUnsupported }
