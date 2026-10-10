//go:build windows

package volume

import (
	"path/filepath"
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
// It needs no administrator rights (see queryStorageProperty).
func SeekPenalty(root string) (hasPenalty bool, known bool) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return false, false
	}
	// DEVICE_SEEK_PENALTY_DESCRIPTOR: Version uint32, Size uint32, IncursSeekPenalty byte (+ padding).
	out, err := queryStorageProperty(vol, propertySeekPenalty, 12)
	if err != nil || len(out) < 9 {
		return false, false
	}
	return out[8] != 0, true
}
