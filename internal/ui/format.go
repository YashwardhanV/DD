// Package ui holds small output helpers shared by every command: human-friendly
// sizes, dates and durations, colors, and the one-line progress display.
//
// Keeping them in one place means every command prints numbers the same way
// ("1.4 MB", "182,334", "41.2s").
package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Thousands formats an integer with thousands separators: 182334 -> "182,334".
func Thousands(n int64) string {
	neg := n < 0
	s := strconv.FormatInt(n, 10)
	if neg {
		s = s[1:]
	}
	// Insert a comma before every group of three digits, from the right.
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// HumanSize formats a byte count the way Explorer does, using 1024-based
// units: 1536 -> "1.5 KB". Negative values mean "unknown" and print as "?".
func HumanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	return HumanSizeU(uint64(n))
}

// HumanSizeU is HumanSize for unsigned sizes (disk capacities).
func HumanSizeU(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	// Rounding can carry into the next unit: 1,048,575 bytes is 1023.999 KB,
	// which must print as "1.0 MB", not "1024 KB".
	if math.Round(v) >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	// Small numbers get one decimal ("4.2 MB"); big ones don't ("212 KB").
	// (99.96 rounds to 100, so it is a big one: "100 KB", not "100.0 KB".)
	if math.Round(v*10) < 1000 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// FormatTime formats a unix timestamp as "2024-03-07 18:22" in local time.
// Zero means "unknown" and prints as "-".
func FormatTime(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
}

// FormatDuration prints a duration compactly: "380ms", "41.2s", "2m03s",
// "1h05m".
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// FormatMillis prints a short duration the way the search footer wants it:
// "21 ms", or "<1 ms" for anything quicker than a millisecond.
func FormatMillis(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// Ago describes how long before `now` the time t was: "just now", "5 minutes
// ago", "3 days ago".
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s ago", unit)
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

// PerSecond returns n/elapsed as a whole number, guarding against division by
// zero for instant operations.
func PerSecond(n int64, elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return n
	}
	return int64(float64(n) / elapsed.Seconds())
}

// Compact abbreviates a count for tables: 950 -> "950", 182334 -> "182k",
// 2400000 -> "2.4M".
func Compact(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}
