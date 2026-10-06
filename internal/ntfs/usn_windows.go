//go:build windows

package ntfs

import "golang.org/x/sys/windows"

// IsElevated reports whether this process is running as Administrator
// ("Run as administrator"). NTFS turbo mode needs it.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
