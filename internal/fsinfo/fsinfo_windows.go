//go:build windows

package fsinfo

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/yashwardhanv/dd/internal/volume"
)

// fileReadOnlyVolume is FILE_READ_ONLY_VOLUME from GetVolumeInformationW.
const fileReadOnlyVolume = 0x00080000

// systemLookup asks Windows about the volume whose root is root (`E:\`).
func systemLookup(root string) (Info, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return Info{}, err
	}
	var fsName [windows.MAX_PATH + 1]uint16
	var serial, maxComponent, flags uint32
	if err := windows.GetVolumeInformation(p, nil, 0, &serial, &maxComponent, &flags, &fsName[0], uint32(len(fsName))); err != nil {
		return Info{}, fmt.Errorf("%w %s: %v", ErrUnknown, root, err)
	}
	i := ForFileSystem(windows.UTF16ToString(fsName[:]))
	i.ReadOnly = flags&fileReadOnlyVolume != 0
	if maxComponent > 0 {
		i.MaxNameUnits = int(maxComponent)
	}

	// The allocation unit: the smallest amount of space any non-empty file takes.
	var spc, bps, freeClusters, totalClusters uint32
	if r, _, _ := procGetDiskFreeSpaceW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&spc)), uintptr(unsafe.Pointer(&bps)),
		uintptr(unsafe.Pointer(&freeClusters)), uintptr(unsafe.Pointer(&totalClusters))); r != 0 && spc > 0 && bps > 0 {
		i.ClusterBytes = int64(spc) * int64(bps)
		i.SectorBytes = int64(bps)
	}
	// The physical sector size matters for unbuffered reads (verify): reads
	// must be aligned to it. Drives with 4 KB sectors that pretend to have 512
	// byte ones are common, so ask the drive itself.
	if len(root) >= 2 && root[1] == ':' {
		if s, err := volume.SectorSize(root[:2]); err == nil && s > i.SectorBytes {
			i.SectorBytes = s
		}
	}
	return i, nil
}

var procGetDiskFreeSpaceW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceW")

// volumeRoot returns the root of the volume that holds path (`E:\`, or a
// mount-point folder such as `C:\Mount\Disk2\`).
func volumeRoot(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var buf [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", fmt.Errorf("%w for %s: %v", ErrUnknown, path, err)
	}
	r := windows.UTF16ToString(buf[:])
	if !strings.HasSuffix(r, `\`) {
		r += `\`
	}
	return r, nil
}

// FileCaseSensitiveInfo (FILE_INFO_BY_HANDLE_CLASS 23) and its flag.
const (
	fileCaseSensitiveInfo   = 23
	fileCSFlagCaseSensitive = 0x1
)

// CaseSensitiveDir reports whether NTFS's per-folder case sensitivity
// (FILE_CASE_SENSITIVE_DIR, Windows 10 1803+) is on for dir. Folders shared
// with WSL often have it. Any failure means "no" (the normal Windows rule).
func CaseSensitiveDir(dir string) bool {
	h, err := openDir(dir, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var flags uint32
	if err := windows.GetFileInformationByHandleEx(h, fileCaseSensitiveInfo, (*byte)(unsafe.Pointer(&flags)), 4); err != nil {
		return false
	}
	return flags&fileCSFlagCaseSensitive != 0
}

// SetCaseSensitiveDir switches per-folder case sensitivity on for a folder
// DriveDex just created (copy --case-sensitive-dest). It needs NTFS and
// Windows 10 1803 or later.
func SetCaseSensitiveDir(dir string) error {
	h, err := openDir(dir, windows.FILE_WRITE_ATTRIBUTES|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	flags := uint32(fileCSFlagCaseSensitive)
	return windows.SetFileInformationByHandle(h, fileCaseSensitiveInfo, (*byte)(unsafe.Pointer(&flags)), 4)
}

func openDir(dir string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}
