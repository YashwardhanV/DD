//go:build !windows

package fsinfo

import "path/filepath"

// systemLookup describes a non-Windows volume. DriveDex only ships for
// Windows; this exists so the tests run anywhere. Linux file systems keep
// nanosecond UTC times and case-sensitive names.
func systemLookup(root string) (Info, error) {
	i := ForFileSystem("posix")
	i.TimeRes = 1
	i.CaseSensitive = true
	return i, nil
}

func volumeRoot(path string) (string, error) {
	if v := filepath.VolumeName(path); v != "" {
		return v + `\`, nil
	}
	return "/", nil
}

// CaseSensitiveDir reports whether NTFS's per-folder case sensitivity is on
// for dir. Always false outside Windows (the whole volume decides there).
func CaseSensitiveDir(dir string) bool { return false }

// SetCaseSensitiveDir is not available outside Windows.
func SetCaseSensitiveDir(dir string) error { return nil }
