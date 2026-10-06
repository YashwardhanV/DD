//go:build !windows

package agent

// driveMask: outside Windows there is no cheap equivalent; nil makes the agent
// do a full drive listing on every poll.
func driveMask() func() (uint32, error) { return nil }
