//go:build windows

package volume

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// ioctlVolumeGetVolumeDiskExtents asks a volume which physical disk(s) it lives on.
const ioctlVolumeGetVolumeDiskExtents = 0x00560000

// queryStorageProperty asks the drive holding vol ("E:") the
// IOCTL_STORAGE_QUERY_PROPERTY question prop and returns the reply.
func queryStorageProperty(vol string, prop uint32, outSize int) ([]byte, error) {
	// STORAGE_PROPERTY_QUERY: PropertyId uint32, QueryType uint32, 4 spare bytes.
	var in [12]byte
	binary.LittleEndian.PutUint32(in[0:], prop)
	binary.LittleEndian.PutUint32(in[4:], queryStandard)
	return queryDisk(vol, ioctlStorageQueryProperty, in[:], outSize)
}

// queryDisk sends the storage IOCTL code to the drive holding vol ("E:") and
// returns the reply.
//
// It asks the volume first. Some storage drivers (Intel RST's iaStorAC, for
// one) answer that with "Incorrect function" although the disk underneath
// knows the answer, so it then asks the physical disk the volume lives on.
// Both are opened with ZERO access rights, so no administrator rights are needed.
func queryDisk(vol string, code uint32, in []byte, outSize int) ([]byte, error) {
	out, err := deviceIoctl(`\\.\`+strings.ToUpper(vol), code, in, outSize)
	if err == nil {
		return out, nil
	}
	disk, derr := volumeDisk(vol)
	if derr != nil {
		return nil, err
	}
	out, perr := deviceIoctl(fmt.Sprintf(`\\.\PhysicalDrive%d`, disk), code, in, outSize)
	if perr != nil {
		return nil, err
	}
	return out, nil
}

// deviceIoctl opens device for questions only and sends it one IOCTL.
func deviceIoctl(device string, code uint32, in []byte, outSize int) ([]byte, error) {
	h, err := openForQuery(device)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)

	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	out := make([]byte, outSize)
	var returned uint32
	if err := windows.DeviceIoControl(h, code, inPtr, uint32(len(in)), &out[0], uint32(len(out)), &returned, nil); err != nil {
		return nil, err
	}
	return out[:returned], nil
}

// volumeDisk returns the number N of the \\.\PhysicalDriveN that vol lives on.
// A volume spread over several disks has no single answer and gives an error.
func volumeDisk(vol string) (uint32, error) {
	h, err := openForQuery(`\\.\` + strings.ToUpper(vol))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)

	// VOLUME_DISK_EXTENTS: NumberOfDiskExtents uint32, 4 padding bytes, then
	// DISK_EXTENT entries of 24 bytes: DiskNumber uint32, 4 padding, StartingOffset, ExtentLength.
	const extentSize = 24
	out := make([]byte, 8+16*extentSize)
	var returned uint32
	if err := windows.DeviceIoControl(h, ioctlVolumeGetVolumeDiskExtents, nil, 0, &out[0], uint32(len(out)), &returned, nil); err != nil {
		return 0, err
	}
	le := binary.LittleEndian
	n := int(le.Uint32(out[0:]))
	if n == 0 || returned < uint32(8+n*extentSize) {
		return 0, errors.New("the volume reported no disk")
	}
	disk := le.Uint32(out[8:])
	for i := 1; i < n; i++ {
		if le.Uint32(out[8+i*extentSize:]) != disk {
			return 0, fmt.Errorf("%s spans %d disks", vol, n)
		}
	}
	return disk, nil
}

// openForQuery opens a device with no access rights - just enough to ask questions.
func openForQuery(device string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(device)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", device, err)
	}
	return h, nil
}
