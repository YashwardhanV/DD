package ui

import (
	"strings"
	"testing"
	"time"
)

func TestThousands(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 182334: "182,334",
		1234567890: "1,234,567,890", -4425: "-4,425",
	}
	for in, want := range cases {
		if got := Thousands(in); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		-1: "?", 0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		212 * 1024: "212 KB", 1468006: "1.4 MB", 18 << 30: "18.0 GB",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		380 * time.Millisecond:                    "380ms",
		41200 * time.Millisecond:                  "41.2s",
		2*time.Minute + 3*time.Second:             "2m03s",
		time.Hour + 5*time.Minute + 9*time.Second: "1h05m",
	}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-1 * time.Minute), "1 minute ago"},
		{now.Add(-5 * time.Minute), "5 minutes ago"},
		{now.Add(-2 * time.Hour), "2 hours ago"},
		{now.Add(-3 * 24 * time.Hour), "3 days ago"},
	}
	for _, c := range cases {
		if got := Ago(c.t, now); got != c.want {
			t.Errorf("Ago(-%v) = %q, want %q", now.Sub(c.t), got, c.want)
		}
	}
}

func TestPerSecond(t *testing.T) {
	if got := PerSecond(1000, 2*time.Second); got != 500 {
		t.Errorf("PerSecond = %d, want 500", got)
	}
	if got := PerSecond(7, 0); got != 7 {
		t.Errorf("PerSecond with zero duration = %d, want 7", got)
	}
}

func TestFormatMillis(t *testing.T) {
	cases := map[time.Duration]string{
		200 * time.Microsecond: "<1 ms", time.Millisecond: "1 ms", 21*time.Millisecond + 700*time.Microsecond: "21 ms",
	}
	for in, want := range cases {
		if got := FormatMillis(in); got != want {
			t.Errorf("FormatMillis(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCompact(t *testing.T) {
	cases := map[int64]string{0: "0", 950: "950", 1000: "1k", 182334: "182k", 999999: "1000k", 2400000: "2.4M"}
	for in, want := range cases {
		if got := Compact(in); got != want {
			t.Errorf("Compact(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatCopyProgress(t *testing.T) {
	got := FormatCopyProgress(CopyProgress{
		FilesDone: 8412, FilesTotal: 12408,
		BytesDone: 13 << 30, BytesTotal: 20 << 30, // 13 / 20 GiB
		Skipped: 3, Failed: 0, BytesPerSec: 86 << 20,
	})
	want := "[ 8,412/12,408 files ]  13.0/20.0 GB  ·  86 MB/s  ·  ETA 1m23s  ·  3 skipped  ·  0 failed"
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if got := FormatCopyProgress(CopyProgress{FilesTotal: 5, BytesTotal: 100}); !strings.Contains(got, "ETA …") || !strings.Contains(got, "0 B/s") {
		t.Errorf("before any speed is known: %q", got)
	}
	if got := FormatCopyProgress(CopyProgress{FilesDone: 5, FilesTotal: 5, BytesDone: 100, BytesTotal: 100, BytesPerSec: 50}); !strings.Contains(got, "ETA 0s") {
		t.Errorf("when finished: %q", got)
	}
}

func TestHumanPairAndRate(t *testing.T) {
	if got := HumanPair(512, 2048); got != "0.5/2.0 KB" {
		t.Errorf("HumanPair = %q", got)
	}
	if got := HumanPair(5, 900); got != "5/900 B" {
		t.Errorf("HumanPair = %q", got)
	}
	cases := map[float64]string{0: "0 B/s", 500: "500 B/s", 4.2 * 1024 * 1024: "4.2 MB/s", 86 * 1024 * 1024: "86 MB/s", 1.5 * 1024 * 1024 * 1024: "1.5 GB/s"}
	for in, want := range cases {
		if got := FormatRate(in); got != want {
			t.Errorf("FormatRate(%v) = %q, want %q", in, got, want)
		}
	}
}

// Rounding used to stop at the unit boundary: "1024 KB" instead of "1.0 MB",
// and "100.0 KB" instead of "100 KB".
func TestHumanSizeRoundsAcrossUnits(t *testing.T) {
	for n, want := range map[int64]string{
		1048575:             "1.0 MB",
		1048524:             "1.0 MB",
		1<<30 - 1:           "1.0 GB",
		102349:              "100 KB",
		102300:              "99.9 KB",
		1023:                "1023 B",
		1024:                "1.0 KB",
		1 << 60:             "1024 PB",
		9223372036854775807: "8192 PB",
	} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
