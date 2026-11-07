//go:build !windows

package ui

import (
	"os"

	"github.com/mattn/go-isatty"
)

// ansiSupported: every non-Windows terminal we care about understands ANSI.
var ansiSupported = true

// SetupConsole prepares the terminal for output. Nothing to do here.
func SetupConsole() (restore func()) { return func() {} }

// termWidth returns the terminal width in columns (COLUMNS env var, or 80).
func termWidth() int {
	return envWidth(os.Getenv("COLUMNS"))
}

// isConsole: a real terminal, not just any character device (/dev/null is
// one too, and is what scripts and services often get as stdin).
func isConsole(f *os.File) bool { return isatty.IsTerminal(f.Fd()) }
