package manifest

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile opens path with no sharing at all: a second DriveDex (on this or
// any other PC) gets a sharing violation until the first closes it. Windows
// releases it when the process dies, so a crash never leaves a stale lock.
func lockFile(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// hideFolder gives the manifest folder the Hidden and System attributes, so
// Explorer doesn't show it by default (best effort).
func hideFolder(dir string) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return
	}
	windows.SetFileAttributes(p, attrs|windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM)
}
