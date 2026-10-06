//go:build !windows

package scan

import (
	"io/fs"
	"strings"

	"github.com/yashwardhanv/dd/internal/index"
)

// attrFlags: on non-Windows systems (only used for development and tests)
// "hidden" simply means the name starts with a dot.
func attrFlags(_ fs.FileInfo, name string) uint8 {
	if strings.HasPrefix(name, ".") {
		return index.FlagHidden
	}
	return 0
}
