//go:build windows

package ntfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// volumeDevice implements Device with a real Windows volume handle.
type volumeDevice struct{ h windows.Handle }

// IOControl implements Device: it calls DeviceIoControl and translates the
// Windows error numbers we care about into this package's sentinel errors.
func (v *volumeDevice) IOControl(code uint32, in, out []byte) (int, error) {
	var inPtr, outPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	if len(out) > 0 {
		outPtr = &out[0]
	}
	var returned uint32
	err := windows.DeviceIoControl(v.h, code, inPtr, uint32(len(in)), outPtr, uint32(len(out)), &returned, nil)
	if err != nil {
		return 0, translate(err)
	}
	return int(returned), nil
}

// translate maps a Windows error to our sentinel errors (keeping the original
// text for --debug).
func translate(err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	switch errno {
	case ErrnoHandleEOF:
		return ErrEOF
	case ErrnoJournalNotActive:
		return fmt.Errorf("%w: %v", ErrJournalNotActive, err)
	case ErrnoJournalDeleteInProg:
		return fmt.Errorf("%w: %v", ErrJournalDeleteInProgress, err)
	case ErrnoJournalEntryDeleted:
		return fmt.Errorf("%w: %v", ErrJournalEntryDeleted, err)
	case ErrnoAccessDenied:
		return fmt.Errorf("%w: %v", ErrNeedAdmin, err)
	}
	return err
}

// Open opens the NTFS volume that holds root (such as `E:\`) for raw reading
// and finds the file reference number of its root folder.
//
// Opening the volume with read access is what needs Administrator rights;
// without them this returns ErrNeedAdmin.
func Open(root string) (*Session, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return nil, fmt.Errorf("%s is not a drive letter", root)
	}
	vol = strings.ToUpper(vol)

	devPath, err := windows.UTF16PtrFromString(`\\.\` + vol)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(devPath, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, translate(fmt.Errorf("opening volume %s: %w", vol, err))
	}

	rootFRN, err := rootFileReference(vol + `\`)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return NewSession(&volumeDevice{h: h}, rootFRN, func() { windows.CloseHandle(h) }), nil
}

// rootFileReference returns the file reference number of a folder (`E:\`).
// USN records name parents by this number, so we need the root's to know which
// entries sit directly in the root.
func rootFileReference(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle.
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, fmt.Errorf("reading the root folder id of %s: %w", dir, err)
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}
