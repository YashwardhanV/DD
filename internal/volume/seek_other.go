//go:build !windows

package volume

import "errors"

// SeekPenalty is unknown outside Windows.
func SeekPenalty(root string) (hasPenalty bool, known bool) { return false, false }

// SectorSize is unknown outside Windows.
func SectorSize(vol string) (int64, error) { return 0, errors.ErrUnsupported }
