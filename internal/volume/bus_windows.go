//go:build windows

package volume

import (
	"fmt"
	"path/filepath"
)

const (
	// propertyDevice is STORAGE_PROPERTY_ID StorageDeviceProperty.
	propertyDevice = 0
	// ioctlStorageGetHotplugInfo asks whether a disk can be unplugged while the PC runs.
	ioctlStorageGetHotplugInfo = 0x002D0C14
)

// BusType asks Windows how the drive holding root is connected (USB, SATA,
// NVMe ...), whether it is hot-pluggable, and what it calls itself. Like
// SeekPenalty it needs no Administrator rights.
func BusType(root string) (BusInfo, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return BusInfo{}, fmt.Errorf("%s is not a drive letter", root)
	}
	out, err := queryStorageProperty(vol, propertyDevice, 1024)
	if err != nil {
		return BusInfo{}, fmt.Errorf("asking %s how it is connected: %w", vol, err)
	}
	info, err := ParseDeviceDescriptor(out)
	if err != nil {
		return info, err
	}
	// Not every driver answers the hotplug question; no answer means "not hot-pluggable".
	if hp, err := queryDisk(vol, ioctlStorageGetHotplugInfo, nil, 8); err == nil {
		info.Hotplug, _ = ParseHotplugInfo(hp)
	}
	return info, nil
}
