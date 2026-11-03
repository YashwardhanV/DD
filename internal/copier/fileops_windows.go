//go:build windows

package copier

import (
	"encoding/binary"
	"errors"
	"os"
	"time"
	"unsafe"

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

// setModTime sets f's modification time through its open handle, the way
// Windows' own CopyFile does. A time set like this is kept when the handle
// closes (the file system doesn't overwrite it with "now").
func setModTime(f *os.File, t time.Time) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, nil // fall back to setting it by name
	}
	ft := windows.NsecToFiletime(t.UnixNano())
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = windows.SetFileTime(windows.Handle(fd), nil, &ft, &ft)
	}); err != nil {
		return false, nil
	}
	if serr != nil {
		return false, serr
	}
	return true, nil
}

// openSourceSingle opens a source file ONCE and reads its size, time and
// attributes from the open handle (BUILD-V2 18a). Go's os.Lstat on Windows
// is a separate lookup by path (GetFileAttributesEx); on a USB drive that is
// one more round trip per file, saved here.
//
// FILE_FLAG_OPEN_REPARSE_POINT opens a link or junction ITSELF instead of
// following it, and the reparse-point attribute then marks it as "not a
// regular file" - the same rule as v1's Lstat check: links are never followed.
// FILE_FLAG_BACKUP_SEMANTICS lets a folder be opened too (it is then refused
// as not regular instead of failing with "access denied").
func openSourceSingle(path string) (*os.File, int64, int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, 0, 0, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, 0, 0, errSourceGone
		}
		return nil, 0, 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, 0, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		windows.CloseHandle(h)
		return nil, 0, 0, errNotRegular
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	mtime := time.Unix(0, info.LastWriteTime.Nanoseconds()).Unix()
	return os.NewFile(uintptr(h), path), size, mtime, nil
}

// openUnbuffered opens a file for reading with FILE_FLAG_NO_BUFFERING.
func openUnbuffered(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// createUnbuffered creates (or truncates) a file for writing with
// FILE_FLAG_NO_BUFFERING.
func createUnbuffered(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.CREATE_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// setEOF sets the file's length with SetFileInformationByHandle
// (FileEndOfFileInfo), which - unlike SetEndOfFile - also accepts a length
// that isn't a whole number of sectors on an unbuffered handle.
func setEOF(f *os.File, size int64) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		eof := size
		serr = windows.SetFileInformationByHandle(windows.Handle(fd), windows.FileEndOfFileInfo, (*byte)(unsafe.Pointer(&eof)), 8)
	}); err != nil {
		return err
	}
	return serr
}
