//go:build !windows

package copier

import (
	"os"
	"time"
)

// preallocate is a no-op off Windows (DriveDex only ships for Windows; the
// tests run everywhere).
func preallocate(*os.File, int64) {}

// setModTime returns false: the caller sets the time by name after closing.
func setModTime(*os.File, time.Time) (bool, error) { return false, nil }
