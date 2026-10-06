//go:build windows

package agent

import "golang.org/x/sys/windows"

// driveMask returns a function reading the bitmask of drive letters in use
// (bit 0 = A:, bit 1 = B:, ...). It is a very cheap call, so the agent can
// make it every two seconds.
func driveMask() func() (uint32, error) {
	return func() (uint32, error) { return windows.GetLogicalDrives() }
}
