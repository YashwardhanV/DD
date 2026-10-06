//go:build !windows

package volume

// SeekPenalty is unknown outside Windows.
func SeekPenalty(root string) (hasPenalty bool, known bool) { return false, false }
