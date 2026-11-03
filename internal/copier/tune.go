package copier

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The autotuner (BUILD-V2 stage 19)
//
// v1 picks the number of parallel workers once - 2 if a hard disk is
// involved, 8 if both ends are flash, 4 if it can't tell - and never changes
// it. Real drives disagree: a cheap pen drive slows DOWN when written by
// several workers at once, a fast SSD wants more, and a drive changes
// behaviour when its own cache fills. So v2 measures and adjusts:
//
//	every 2 s: score = files/s (small files) or MB/s (big files)
//	try 2 more workers -> better by > 5%? keep them and try 2 more
//	                   -> not better? go back, then try 1 fewer
//	neither direction wins -> settle; look again every 30 s
//
// Workers take a slot from a Limit before each file, so the limit can shrink
// or grow at any moment; shrinking never interrupts a file in progress (a
// worker that is over the new limit just waits before its next file).

// Limit is a resizable semaphore.
type Limit struct {
	mu    sync.Mutex
	cond  *sync.Cond
	n     int
	inUse int
}

// NewLimit returns a Limit allowing n holders (at least 1).
func NewLimit(n int) *Limit {
	l := &Limit{n: max(n, 1)}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Acquire waits for a free slot. It returns ctx's error if ctx ends first.
func (l *Limit) Acquire(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.inUse >= l.n {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.inUse++
	return nil
}

// Release gives a slot back.
func (l *Limit) Release() {
	l.mu.Lock()
	l.inUse--
	l.mu.Unlock()
	l.cond.Broadcast()
}

// Set changes the number of slots (at least 1).
func (l *Limit) Set(n int) {
	l.mu.Lock()
	l.n = max(n, 1)
	l.mu.Unlock()
	l.cond.Broadcast()
}

// Value is the current number of slots.
func (l *Limit) Value() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// InUse is how many slots are taken right now.
func (l *Limit) InUse() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}

// TuneOptions turns the autotuner on for a copy (Options.Tune).
type TuneOptions struct {
	Min, Max int           // see TuneBounds
	Window   time.Duration // 0 = 2 s
}

// Tuner hill-climbs the worker count.
type Tuner struct {
	Min, Max, Start int
	Window          time.Duration // 2 s
	Hysteresis      float64       // 0.05 = a change must win by 5% to be kept
	Reprobe         time.Duration // 30 s: look again after settling

	// ticks replaces the window timer in tests.
	ticks <-chan time.Time
}

// Tuning defaults.
const (
	tuneWindow     = 2 * time.Second
	tuneHysteresis = 0.05
	tuneReprobe    = 30 * time.Second
	tuneUpStep     = 2
	tuneHistory    = 3       // windows kept per worker count (their median is the score)
	smallFileScore = 1 << 20 // windows averaging smaller files are scored in files/s
)

// TuneBounds returns the tuner's bounds: at most 4 workers when either end
// is a spinning disk, else 16.
func TuneBounds(seekPenalty bool) (minW, maxW int) {
	if seekPenalty {
		return 1, 4
	}
	return 1, 16
}

type tunePhase int

const (
	phaseBase tunePhase = iota // measuring the current setting
	phaseUp                    // trying +2
	phaseDown                  // trying -1
	phaseSettled
)

// Run adjusts l every window until ctx ends. sample returns the running
// totals of files and bytes copied; log receives one line per change
// ("workers 8 -> 10: +7% files/s").
func (t *Tuner) Run(ctx context.Context, l *Limit, sample func() (files, bytes int64), log func(string)) {
	window := t.Window
	if window <= 0 {
		window = tuneWindow
	}
	hyst := t.Hysteresis
	if hyst <= 0 {
		hyst = tuneHysteresis
	}
	reprobe := t.Reprobe
	if reprobe <= 0 {
		reprobe = tuneReprobe
	}
	minW, maxW := max(t.Min, 1), max(t.Max, 1)
	if maxW < minW {
		maxW = minW
	}
	clamp := func(n int) int { return min(max(n, minW), maxW) }
	ticks := t.ticks
	if ticks == nil {
		tk := time.NewTicker(window)
		defer tk.Stop()
		ticks = tk.C
	}

	cur := clamp(t.Start)
	l.Set(cur)
	hist := map[int][]float64{}
	score := func(n int) float64 { return median(hist[n]) }
	phase, cand := phaseBase, cur
	smallMode := false
	windows, settledAt := 0, 0
	lastF, lastB := sample()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		f, b := sample()
		df, db := f-lastF, b-lastB
		lastF, lastB = f, b
		if df <= 0 && db <= 0 {
			continue // nothing finished this window (a big file in progress): no information
		}
		windows++
		small := df > 0 && db/max(df, 1) < smallFileScore
		if small != smallMode {
			// The mix changed (largest files go first, so big files end and
			// small ones begin): scores in files/s and MB/s can't be compared.
			smallMode = small
			hist = map[int][]float64{}
			l.Set(cur)
			phase, cand = phaseBase, cur
		}
		var s float64
		if smallMode {
			s = float64(df) / window.Seconds()
		} else {
			s = float64(db) / window.Seconds()
		}
		at := l.Value()
		hist[at] = append(hist[at], s)
		if len(hist[at]) > tuneHistory {
			hist[at] = hist[at][len(hist[at])-tuneHistory:]
		}
		unit := "MB/s"
		if smallMode {
			unit = "files/s"
		}
		better := func() bool { return score(cand) > score(cur)*(1+hyst) }
		accept := func() {
			if log != nil {
				gain := 0.0
				if sc := score(cur); sc > 0 {
					gain = (score(cand)/sc - 1) * 100
				}
				log(fmt.Sprintf("workers %d -> %d: %+.0f%% %s", cur, cand, gain, unit))
			}
			cur = cand
		}
		next := func(p tunePhase) {
			phase = p
			switch p {
			case phaseUp:
				cand = clamp(cur + tuneUpStep)
			case phaseDown:
				cand = clamp(cur - 1)
			}
			if (p == phaseUp || p == phaseDown) && cand == cur {
				if p == phaseUp {
					p, cand = phaseDown, clamp(cur-1)
					phase = p
				}
				if cand == cur {
					phase, settledAt = phaseSettled, windows
					l.Set(cur)
					return
				}
			}
			if phase == phaseSettled {
				l.Set(cur)
				settledAt = windows
				return
			}
			l.Set(cand)
		}

		switch phase {
		case phaseBase:
			next(phaseUp)
		case phaseUp:
			if better() {
				accept()
				next(phaseUp)
			} else {
				l.Set(cur)
				next(phaseDown)
			}
		case phaseDown:
			if better() {
				accept()
				next(phaseDown)
			} else {
				l.Set(cur)
				next(phaseSettled)
			}
		case phaseSettled:
			// Drives change behaviour (a full cache, a hot controller): look again now and then.
			if time.Duration(windows-settledAt)*window >= reprobe {
				next(phaseUp)
			}
		}
	}
}

// median of a few scores.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
