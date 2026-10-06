package ui

import (
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
