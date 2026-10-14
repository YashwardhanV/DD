//go:build windows

package copier

import (
	"encoding/binary"
	"os"
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
