//go:build windows

package volume

import (
	"fmt"
	"path/filepath"
)

// propertyDevice is STORAGE_PROPERTY_ID StorageDeviceProperty.
const propertyDevice = 0

// BusType asks Windows how the drive holding root is connected (USB, SATA,
// NVMe ...) and what it calls itself. Like SeekPenalty it needs no
// Administrator rights.
func BusType(root string) (BusInfo, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return BusInfo{}, fmt.Errorf("%s is not a drive letter", root)
	}
	out, err := queryStorageProperty(vol, propertyDevice, 1024)
	if err != nil {
		return BusInfo{}, fmt.Errorf("asking %s how it is connected: %w", vol, err)
	}
	return ParseDeviceDescriptor(out)
}
