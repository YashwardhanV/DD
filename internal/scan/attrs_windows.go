//go:build windows

package scan

import (
	"io/fs"
	"syscall"

	"github.com/yashwardhanv/dd/internal/index"
)

// attrFlags converts Windows file attributes into index flags.
//
// On Windows the attributes arrive for free with the directory listing (the
// same call that gave us the name), so reading them costs no extra I/O.
func attrFlags(info fs.FileInfo, _ string) uint8 {
	var f uint8
	if sys, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		if sys.FileAttributes&syscall.FILE_ATTRIBUTE_HIDDEN != 0 {
			f |= index.FlagHidden
		}
		if sys.FileAttributes&syscall.FILE_ATTRIBUTE_SYSTEM != 0 {
			f |= index.FlagSystem
		}
	}
	return f
}
