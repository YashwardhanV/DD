package ui

import (
	"fmt"
	"math"
	"time"
)

// CopyProgress is a snapshot of a running copy, for the one-line display.
type CopyProgress struct {
	FilesDone, FilesTotal int64
	BytesDone, BytesTotal int64
	Skipped, Failed       int64
	BytesPerSec           float64 // smoothed speed; 0 = not known yet
}

// FormatCopyProgress renders the copy status line:
//
//	[ 8,412/12,408 files ]  12.1/18.3 GB  ·  86 MB/s  ·  ETA 1m12s  ·  3 skipped  ·  0 failed
func FormatCopyProgress(p CopyProgress) string {
	eta := "ETA …"
	if p.BytesPerSec > 0 && p.BytesTotal > p.BytesDone {
		remaining := float64(p.BytesTotal-p.BytesDone) / p.BytesPerSec
		eta = "ETA " + FormatDuration(time.Duration(remaining*float64(time.Second)).Round(time.Second))
	} else if p.BytesTotal <= p.BytesDone {
		eta = "ETA 0s"
	}
	return fmt.Sprintf("[ %s/%s files ]  %s  ·  %s  ·  %s  ·  %d skipped  ·  %d failed",
		Thousands(p.FilesDone), Thousands(p.FilesTotal),
		HumanPair(p.BytesDone, p.BytesTotal), FormatRate(p.BytesPerSec), eta, p.Skipped, p.Failed)
}

// HumanPair formats "done/total" in the unit of total: 12.1/18.3 GB.
func HumanPair(done, total int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	scale := 1.0
	for float64(total)/scale >= 1024 && i < len(units)-1 {
		scale *= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d/%d B", done, total)
	}
	return fmt.Sprintf("%.1f/%.1f %s", float64(done)/scale, float64(total)/scale, units[i])
}

// FormatRate formats a speed in bytes per second: "86 MB/s", "4.2 MB/s", "0 B/s".
func FormatRate(bytesPerSec float64) string {
	if bytesPerSec < 1 || math.IsNaN(bytesPerSec) || math.IsInf(bytesPerSec, 0) {
		return "0 B/s"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := bytesPerSec
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 || v >= 10 {
		return fmt.Sprintf("%.0f %s/s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s/s", v, units[i])
}
