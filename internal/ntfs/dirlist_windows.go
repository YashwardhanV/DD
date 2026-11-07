//go:build windows

package ntfs

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// FILE_INFO_BY_HANDLE_CLASS values for folder listings.
const (
	fileIDBothDirectoryInfo        = 10
	fileIDBothDirectoryRestartInfo = 11
)

// WindowsLister lists folders with GetFileInformationByHandleEx
// (FileIdBothDirectoryInfo): one call returns as many entries as fit in its
// 64 KiB buffer, each with its file reference number, size and times. No
// Administrator rights are needed.
type WindowsLister struct{}

// ListDir implements DirLister.
func (WindowsLister) ListDir(dir string) ([]DirRecord, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, fmt.Errorf("opening folder %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)

	buf := make([]byte, 64<<10)
	var recs []DirRecord
	class := uint32(fileIDBothDirectoryRestartInfo)
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return recs, nil
		}
		if err != nil {
			return recs, fmt.Errorf("listing folder %s: %w", dir, err)
		}
		class = fileIDBothDirectoryInfo
		if recs, err = ParseDirInfo(recs, buf); err != nil {
			return recs, err
		}
	}
}
