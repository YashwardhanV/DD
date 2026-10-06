//go:build !windows

package tui

import (
	"errors"

	"github.com/atotto/clipboard"
)

// DefaultActions outside Windows: only the clipboard works. (DriveDex is a
// Windows tool; this exists so the package builds and can be tested anywhere.)
func DefaultActions() Actions {
	unsupported := func(string) error { return errors.ErrUnsupported }
	return Actions{Open: unsupported, ShowInFolder: unsupported, CopyText: clipboard.WriteAll}
}
