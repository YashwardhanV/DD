//go:build windows

package volume

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// InitProcess must run once at program start, before any drive is touched.
//
// SEM_FAILCRITICALERRORS and SEM_NOOPENFILEERRORBOX tell Windows "don't show a
// 'There is no disk in the drive' pop-up; just return an error". Without this,
// listing drives would pop a dialog for every empty card-reader slot.
func InitProcess() {
	windows.SetErrorMode(windows.SEM_FAILCRITICALERRORS | windows.SEM_NOOPENFILEERRORBOX)
}

// List returns every removable, fixed and network drive (CD drives and
// unassigned letters are skipped). Drives that can't be read right now are
// included with Ready=false.
func List() ([]Volume, error) { return list(true) }

// ListLocal is List without network drives. A disconnected network share can
// make Windows wait many seconds when asked for its label, so anything that
// polls (the background agent) must not touch them.
func ListLocal() ([]Volume, error) { return list(false) }

func list(includeRemote bool) ([]Volume, error) {
	// A bitmask: bit 0 = A:, bit 1 = B:, ... bit 25 = Z:.
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("listing drives: %w", err)
	}
	var vols []Volume
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		if v, ok := query(string(rune('A'+i))+`:\`, includeRemote); ok {
			vols = append(vols, v)
		}
	}
	return vols, nil
}

// ForPath returns the volume that contains path.
func ForPath(path string) (Volume, error) {
	vol := filepath.VolumeName(path)
	if len(vol) != 2 || vol[1] != ':' {
		return Volume{}, fmt.Errorf("%s is not on a drive letter (network shares are not supported here)", path)
	}
	v, ok := query(strings.ToUpper(vol)+`\`, true)
	if !ok {
		return Volume{}, fmt.Errorf("drive %s is not a removable, fixed or network drive", vol)
	}
	return v, nil
}

// query reads everything we need about one drive root such as `E:\`.
// ok is false for drive types we ignore (CD-ROM, RAM disk, nothing mounted).
func query(root string, includeRemote bool) (v Volume, ok bool) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return v, false
	}
	dt := windows.GetDriveType(rootPtr)
	switch {
	case dt == windows.DRIVE_REMOVABLE, dt == windows.DRIVE_FIXED:
	case dt == windows.DRIVE_REMOTE && includeRemote:
	default:
		return v, false
	}
	v = Volume{Root: root, DriveType: dt}

	var label, fsName [windows.MAX_PATH + 1]uint16
	var serial, maxComponent, flags uint32
	err = windows.GetVolumeInformation(rootPtr, &label[0], uint32(len(label)),
		&serial, &maxComponent, &flags, &fsName[0], uint32(len(fsName)))
	if err != nil {
		return v, true // exists but not ready (e.g. empty card reader)
	}
	v.Ready = true
	v.Label = windows.UTF16ToString(label[:])
	v.FileSystem = windows.UTF16ToString(fsName[:])
	v.Serial = serial

	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(rootPtr, &freeToCaller, &total, &totalFree); err == nil {
		v.TotalBytes, v.FreeBytes = total, freeToCaller
	}
	return v, true
}
