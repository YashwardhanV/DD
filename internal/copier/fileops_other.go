//go:build !windows

package copier

import (
	"os"
	"time"
)

// preallocate is a no-op off Windows (DriveDex only ships for Windows; the
// tests run everywhere).
func preallocate(*os.File, int64) {}

// setFileTimes returns false: the caller sets the time by name after closing.
func setFileTimes(*os.File, time.Time, time.Time) (bool, error) { return false, nil }

// creationTime is unknown off Windows.
func creationTime(os.FileInfo) time.Time { return time.Time{} }

// lockedOpen is a normal open off Windows (no share modes).
func lockedOpen(name string) (*os.File, error) { return os.Open(name) }

// unbufferedFlags is 0 off Windows.
const unbufferedFlags = 0

// unbufferedOpen is a normal open off Windows (tests only).
func unbufferedOpen(name string) (*os.File, error) { return os.Open(name) }

// platformFirstLCN is unknown off Windows.
func platformFirstLCN(string) (int64, bool) { return 0, false }

// platformFileID is unknown off Windows.
func platformFileID(*os.File) uint64 { return 0 }
