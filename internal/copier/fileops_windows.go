//go:build windows

package copier

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// preallocate reserves size bytes for f (FILE_ALLOCATION_INFO). It changes
// how much space the file system sets aside, not the file's size, so a copy
// that is interrupted still leaves a .ddxpart of the length actually
// written; Windows gives back any unused reservation when the file is closed.
func preallocate(f *os.File, size int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(size))
	if rc, err := f.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			_ = windows.SetFileInformationByHandle(windows.Handle(fd), windows.FileAllocationInfo, &buf[0], uint32(len(buf)))
		})
	}
}

// setFileTimes sets f's modification time (and creation time, when known)
// through its open handle, the way Windows' own CopyFile does. A time set like
// this is kept when the handle closes (the file system doesn't overwrite it
// with "now"), and it is written at full precision: 100 ns on NTFS.
func setFileTimes(f *os.File, mtime, ctime time.Time) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, nil // fall back to setting it by name
	}
	ft := windows.NsecToFiletime(mtime.UnixNano())
	var cft *windows.Filetime
	if !ctime.IsZero() {
		c := windows.NsecToFiletime(ctime.UnixNano())
		cft = &c
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = windows.SetFileTime(windows.Handle(fd), cft, &ft, &ft)
	}); err != nil {
		return false, nil
	}
	if serr != nil {
		return false, serr
	}
	return true, nil
}

// creationTime is the file's creation time (Windows keeps one; it is copied
// along because people expect it, and it costs nothing on the same handle).
func creationTime(fi os.FileInfo) time.Time {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && d != nil {
		return time.Unix(0, d.CreationTime.Nanoseconds())
	}
	return time.Time{}
}

// lockedOpen opens a source file for reading while letting other programs
// read it but NOT write to it (share mode FILE_SHARE_READ). If another program
// already has it open for writing, Windows refuses with a sharing violation:
// that file is "in use", and copying it now would capture a half-written state.
func lockedOpen(name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errInUse
		}
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}

// unbufferedFlags are the flags the verify read-back opens a copy with:
// FILE_FLAG_NO_BUFFERING bypasses the Windows file cache, so the bytes come
// from the drive; SEQUENTIAL_SCAN lets the drive read ahead.
const unbufferedFlags = windows.FILE_FLAG_NO_BUFFERING | windows.FILE_FLAG_SEQUENTIAL_SCAN

// unbufferedOpen opens a file for reading without the Windows cache.
func unbufferedOpen(name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, unbufferedFlags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}

// fsctlGetRetrievalPointers asks NTFS where a file's clusters are.
const fsctlGetRetrievalPointers = 0x00090073

// platformFirstLCN returns the logical cluster number of a file's first
// extent. It opens the file only to read attributes (no Administrator rights
// needed). Files stored inside their MFT record have no extent: false.
func platformFirstLCN(path string) (int64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var in [8]byte // STARTING_VCN_INPUT_BUFFER: StartingVcn = 0
	// RETRIEVAL_POINTERS_BUFFER: ExtentCount uint32, 4 padding, StartingVcn
	// int64, then {NextVcn, Lcn int64} per extent. Room for one is enough.
	var out [32]byte
	var n uint32
	err = windows.DeviceIoControl(h, fsctlGetRetrievalPointers, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &n, nil)
	if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return 0, false
	}
	if n < 32 || binary.LittleEndian.Uint32(out[0:]) == 0 {
		return 0, false
	}
	lcn := int64(binary.LittleEndian.Uint64(out[24:]))
	return lcn, lcn >= 0
}

// platformFileID is the NTFS file reference number of an open file (the
// FileIndex of GetFileInformationByHandle), 0 if unknown. It is the same
// number the change journal and the saved index use for the file.
func platformFileID(f *os.File) uint64 {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
