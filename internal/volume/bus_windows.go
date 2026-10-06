//go:build windows

package volume

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// propertyDevice is STORAGE_PROPERTY_ID StorageDeviceProperty.
const propertyDevice = 0

// BusType asks Windows how the drive holding root is connected (USB, SATA,
// NVMe ...) and what it calls itself. Like SeekPenalty it opens the volume
// with no access rights, so no Administrator rights are needed.
func BusType(root string) (BusInfo, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return BusInfo{}, fmt.Errorf("%s is not a drive letter", root)
	}
	devPath, err := windows.UTF16PtrFromString(`\\.\` + strings.ToUpper(vol))
	if err != nil {
		return BusInfo{}, err
	}
	h, err := windows.CreateFile(devPath, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return BusInfo{}, fmt.Errorf("opening %s: %w", vol, err)
	}
	defer windows.CloseHandle(h)

	var in [12]byte // STORAGE_PROPERTY_QUERY: PropertyId, QueryType, 4 spare bytes
	binary.LittleEndian.PutUint32(in[0:], propertyDevice)
	binary.LittleEndian.PutUint32(in[4:], queryStandard)

	out := make([]byte, 1024)
	var returned uint32
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &returned, nil); err != nil {
		return BusInfo{}, fmt.Errorf("asking %s how it is connected: %w", vol, err)
	}
	return ParseDeviceDescriptor(out[:returned])
}
