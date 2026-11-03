package copier

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// simDevice is a pretend drive: its speed depends on how many workers write
// at once, with a known best worker count, plus a little noise.
type simDevice struct {
	mu      sync.Mutex
	rng     *rand.Rand
	opt     float64
	l       *Limit
	files   int64
	bytes   int64
	fileLen int64
	history []int
}

// rate is files per window at n workers: a peak at opt, falling off on both sides.
func (d *simDevice) rate(n int) float64 {
	x := float64(n)
	return 50 + 1000*math.Exp(-math.Pow((x-d.opt)/4, 2)) // even a bad setting copies something
}

func (d *simDevice) sample() (int64, int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.l.Value()
	d.history = append(d.history, n)
	r := d.rate(n) * (1 + (d.rng.Float64()-0.5)*0.02) // ±1% noise
	d.files += int64(r)
	d.bytes += int64(r) * d.fileLen
	return d.files, d.bytes
}

func runSim(t *testing.T, start, minW, maxW int, opt float64, windows int) (*simDevice, []string) {
	t.Helper()
	l := NewLimit(start)
	d := &simDevice{rng: rand.New(rand.NewSource(int64(opt * 100))), opt: opt, l: l, fileLen: 100 << 10}
	ticks := make(chan time.Time)
	tu := &Tuner{Min: minW, Max: maxW, Start: start, Window: 2 * time.Second, ticks: ticks}
	ctx, cancel := context.WithCancel(context.Background())
	var logs []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		tu.Run(ctx, l, d.sample, func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() })
		close(done)
	}()
	for i := 0; i < windows; i++ {
		ticks <- time.Now()
	}
	cancel()
	<-done
	return d, logs
}

func TestTunerFindsTheBestWorkerCount(t *testing.T) {
	for _, c := range []struct {
		start, max int
		opt        float64
	}{{4, 16, 8}, {8, 16, 3}, {2, 4, 3}, {8, 16, 14}, {4, 16, 4}} {
		d, logs := runSim(t, c.start, 1, c.max, c.opt, 9) // fewer than 10 windows
		got := d.l.Value()
		if math.Abs(float64(got)-c.opt) > 1 {
			t.Errorf("start %d, best %v: settled at %d (windows %v, log %q)", c.start, c.opt, got, d.history, logs)
		}
	}
}

func TestTunerRespectsBoundsAndLogsChanges(t *testing.T) {
	d, logs := runSim(t, 2, 1, 4, 12, 12) // the best (12) is above Max (4)
	if d.l.Value() != 4 {
		t.Errorf("settled at %d, want the bound 4", d.l.Value())
	}
	for _, n := range d.history {
		if n < 1 || n > 4 {
			t.Fatalf("went outside the bounds: %v", d.history)
		}
	}
	if len(logs) == 0 || logs[0] != "workers 2 -> 4: +"+logs[0][len("workers 2 -> 4: +"):] {
		t.Errorf("log %q", logs)
	}
}

func TestTunerReprobesAfterSettling(t *testing.T) {
	l := NewLimit(4)
	ticks := make(chan time.Time)
	best := atomic.Int64{}
	best.Store(4)
	var files int64
	sample := func() (int64, int64) {
		n := float64(l.Value())
		files += int64(1000 * math.Exp(-math.Pow((n-float64(best.Load()))/3, 2)))
		return files, files * 1000
	}
	tu := &Tuner{Min: 1, Max: 16, Start: 4, Window: 2 * time.Second, Reprobe: 30 * time.Second, ticks: ticks}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tu.Run(ctx, l, sample, nil); close(done) }()
	for i := 0; i < 10; i++ {
		ticks <- time.Now()
	}
	best.Store(10) // the drive changed (e.g. its cache emptied)
	for i := 0; i < 40; i++ {
		ticks <- time.Now()
	}
	cancel()
	<-done
	if v := l.Value(); v < 9 || v > 11 {
		t.Errorf("after the drive changed the tuner is at %d, want about 10", v)
	}
}

func TestLimitShrinkingNeverInterruptsAHolder(t *testing.T) {
	l := NewLimit(5)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := l.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	l.Set(2)
	got := make(chan struct{})
	go func() { l.Acquire(ctx); close(got) }()
	for i := 0; i < 3; i++ {
		l.Release() // 4, 3, 2 in use: still at or over the new limit
		select {
		case <-got:
			t.Fatalf("acquired with %d in use and a limit of 2", l.InUse())
		case <-time.After(5 * time.Millisecond):
		}
	}
	l.Release() // 1 in use
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("never acquired after dropping below the limit")
	}
	cctx, cancel := context.WithCancel(ctx)
	l.Set(1)
	errc := make(chan error)
	go func() { errc <- l.Acquire(cctx) }()
	cancel()
	if err := <-errc; err == nil {
		t.Error("a cancelled Acquire must return the context's error")
	}
}

func TestTunedCopyCopiesEverything(t *testing.T) {
	src, res := source(t, 300)
	dst := t.TempDir()
	opt := fastOpts(4)
	opt.Tune = &TuneOptions{Min: 1, Max: 16, Window: time.Millisecond}
	r := mustRun(t, plan(t, src, dst, OnConflictNewer), opt)
	if r.Written != int64(res.Files) || r.Failed != 0 {
		t.Fatalf("%+v", r)
	}
	leftovers(t, dst)
	// Cancelled while tuning: stops cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	afterCopy = func(string) { cancel() }
	defer func() { afterCopy = nil }()
	r, err := Run(ctx, plan(t, src, t.TempDir(), OnConflictNewer), opt)
	if err != nil || !r.Cancelled {
		t.Errorf("%v %+v", err, r)
	}
}
