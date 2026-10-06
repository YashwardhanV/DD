//go:build windows

package volume

import (
	"encoding/binary"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	// ioctlStorageQueryProperty asks a storage driver a question about itself.
	ioctlStorageQueryProperty = 0x002D1400
	// propertySeekPenalty is STORAGE_PROPERTY_ID StorageDeviceSeekPenaltyProperty.
	propertySeekPenalty = 7
	// queryStandard is STORAGE_QUERY_TYPE PropertyStandardQuery.
	queryStandard = 0
)

// SeekPenalty reports whether the drive holding root is a spinning hard disk
// ("seek penalty": the arm has to move). known is false when Windows can't
// tell us, which is common with some USB enclosures.
//
// It opens the volume with ZERO access rights - just enough to ask questions -
// so it needs no administrator rights.
func SeekPenalty(root string) (hasPenalty bool, known bool) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return false, false
	}
	devPath, err := windows.UTF16PtrFromString(`\\.\` + strings.ToUpper(vol))
	if err != nil {
		return false, false
	}
	h, err := windows.CreateFile(devPath, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false, false
	}
	defer windows.CloseHandle(h)

	// STORAGE_PROPERTY_QUERY: PropertyId uint32, QueryType uint32, 4 spare bytes.
	var in [12]byte
	binary.LittleEndian.PutUint32(in[0:], propertySeekPenalty)
	binary.LittleEndian.PutUint32(in[4:], queryStandard)

	// DEVICE_SEEK_PENALTY_DESCRIPTOR: Version uint32, Size uint32, IncursSeekPenalty byte (+ padding).
	var out [12]byte
	var returned uint32
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &returned, nil); err != nil {
		return false, false
	}
	if returned < 9 {
		return false, false
	}
	return out[8] != 0, true
}
