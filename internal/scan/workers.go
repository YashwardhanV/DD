package scan

import "runtime"

// AutoWorkers picks how many goroutines should read directories at once.
//
// Why it depends on the drive:
//   - A spinning hard disk has ONE moving arm. Many parallel readers make it
//     jump back and forth between folders, which is slower than reading one
//     folder after another. So we use just 2 (enough to overlap a little
//     CPU work with the disk).
//   - Flash drives and SSDs have no arm; they serve many requests at the same
//     time, so more readers means more throughput.
//   - If Windows won't tell us (some USB enclosures hide it) we pick a safe
//     middle value.
func AutoWorkers(hasSeekPenalty, known bool) (workers int, why string) {
	switch {
	case !known:
		return 4, "drive type unknown"
	case hasSeekPenalty:
		return 2, "spinning hard disk detected"
	default:
		n := 2 * runtime.NumCPU()
		if n > 16 {
			n = 16
		}
		return n, "flash/SSD detected"
	}
}
