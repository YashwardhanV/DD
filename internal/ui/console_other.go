//go:build !windows

package ui

import "os"

// ansiSupported: every non-Windows terminal we care about understands ANSI.
var ansiSupported = true

// SetupConsole prepares the terminal for output. Nothing to do here.
func SetupConsole() (restore func()) { return func() {} }

// termWidth returns the terminal width in columns (COLUMNS env var, or 80).
func termWidth() int {
	return envWidth(os.Getenv("COLUMNS"))
}
