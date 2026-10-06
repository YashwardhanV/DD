//go:build windows

package copier

import (
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/yashwardhanv/dd/internal/volume"
)

// diskFree returns the free bytes available to the current user on the drive holding path.
func diskFree(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("checking free space on %s: %w", path, err)
	}
	return freeToCaller, nil
}

// fileSystemOf returns the file system name ("NTFS", "FAT32", "exFAT") of the
// drive holding path.
func fileSystemOf(path string) (string, error) {
	v, err := volume.ForPath(path)
	if err != nil {
		return "", err
	}
	return v.FileSystem, nil
}
