package trace

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNilRecorderIsSafeAndFree(t *testing.T) {
	var r *Recorder
	m := r.Start()
	r.End(Data, m)
	r.Add(PlanList, time.Second)
	r.FileDone(time.Millisecond, 10)
	r.Note("x")
	rep := r.Report(time.Second, Context{})
	if rep.Files != 0 || len(rep.Phases) != 0 || rep.Hints == nil {
		t.Fatalf("nil recorder report: %+v", rep)
	}
	if n := testing.AllocsPerRun(1000, func() { r.End(Data, r.Start()); r.FileDone(1, 1) }); n != 0 {
		t.Errorf("a nil recorder allocates %v times per call", n)
	}
}

func TestBuckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0}, {500 * time.Nanosecond, 0}, {time.Microsecond, 1}, {3 * time.Microsecond, 2},
		{4 * time.Microsecond, 3}, {time.Millisecond, 10}, {time.Second, 20}, {time.Hour, histBuckets - 1},
	}
	for _, c := range cases {
		if got := bucket(c.d); got != c.want {
			t.Errorf("bucket(%v) = %d, want %d", c.d, got, c.want)
		}
		if c.d > 0 && c.want < histBuckets-1 && bucketUpper(c.want) < c.d {
			t.Errorf("bucketUpper(%d) = %v is below %v", c.want, bucketUpper(c.want), c.d)
		}
	}
}

func TestPercentiles(t *testing.T) {
	r := New()
	for i := 0; i < 90; i++ {
		r.FileDone(8*time.Millisecond, 1)
	}
	for i := 0; i < 10; i++ {
		r.FileDone(60*time.Millisecond, 1)
	}
	rep := r.Report(time.Second, Context{})
	if rep.P50 < 8*time.Millisecond || rep.P50 > 17*time.Millisecond {
		t.Errorf("p50 = %v, want about 8-16 ms", rep.P50)
	}
	if rep.P95 < 60*time.Millisecond || rep.P95 > 140*time.Millisecond {
		t.Errorf("p95 = %v, want about 60-131 ms", rep.P95)
	}
	if rep.TimeToFirstFile <= 0 {
		t.Error("time to first file not recorded")
	}
}

func TestPhasesAddUpAcrossGoroutines(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.Add(Data, time.Microsecond)
				r.Add(Rename, time.Microsecond)
				r.FileDone(2*time.Microsecond, 100)
			}
		}()
	}
	wg.Wait()
	rep := r.Report(10*time.Millisecond, Context{Workers: 8})
	if rep.Files != 8000 || rep.Bytes != 800000 {
		t.Fatalf("files %d bytes %d", rep.Files, rep.Bytes)
	}
	if rep.Time(Data) != 8*time.Millisecond || rep.Share(Data) != 0.5 {
		t.Errorf("data %v share %v", rep.Time(Data), rep.Share(Data))
	}
	var n int64
	for _, c := range rep.Histogram {
		n += c
	}
	if n != 8000 {
		t.Errorf("histogram holds %d files", n)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	r := New()
	r.Add(PlanList, 41*time.Second)
	r.Add(Data, 2*time.Second)
	r.FileDone(time.Millisecond, 1<<20)
	r.Note("workers 8 -> 10: +7% files/s")
	rep := r.Report(60*time.Second, Context{SrcKind: "usb-flash", Workers: 8, PlanSource: "walk"})
	p := filepath.Join(t.TempDir(), "t.json")
	if err := rep.WriteJSONFile(p); err != nil {
		t.Fatal(err)
	}
	back, err := ReadJSONFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Files != rep.Files || back.Time(PlanList) != 41*time.Second || back.Context.PlanSource != "walk" ||
		len(back.Hints) != len(rep.Hints) || len(back.Notes) != 1 || back.P50 != rep.P50 {
		t.Errorf("round trip lost data:\n%+v\n%+v", rep, back)
	}
}

func TestWriteTextShowsTheTable(t *testing.T) {
	r := New()
	r.Add(PlanList, 41*time.Second)
	r.Add(DstCreate, 88*time.Second)
	r.Add(Data, 122*time.Second)
	for i := 0; i < 100; i++ {
		r.FileDone(9*time.Millisecond, 340<<10)
	}
	var b bytes.Buffer
	r.Report(2*time.Minute, Context{Workers: 8, PlanSource: "walk"}).WriteText(&b)
	for _, want := range []string{"Where the time went (8 workers, plan: walk)", "listing the source", "41.0 s", "creating files (.ddxpart)", "Per-file time: p50", "Hints:"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}

// report builds a Report with the given phase times and totals, the way
// Recorder.Report would.
func report(elapsed time.Duration, files, bytes int64, ctx Context, phases map[Phase]time.Duration) Report {
	r := New()
	for p, d := range phases {
		r.Add(p, d)
	}
	for i := int64(0); i < files; i++ {
		r.FileDone(time.Millisecond, 0)
	}
	r.bytes.Store(bytes)
	return r.Report(elapsed, ctx)
}

func has(hints []string, word string) bool {
	for _, h := range hints {
		if strings.Contains(h, word) {
			return true
		}
	}
	return false
}

func TestHintLinkBound(t *testing.T) {
	ctx := Context{LinkMBps: 40}
	// 35 MB/s over a 40 MB/s link, 80% in Data: fires.
	rep := report(10*time.Second, 10, 350e6, ctx, map[Phase]time.Duration{Data: 8 * time.Second, DstCreate: time.Second})
	if !has(rep.Hints, "USB link is the limit") || !has(rep.Hints, "USB 2.0 port?") {
		t.Errorf("should fire: %q", rep.Hints)
	}
	// Same, but only 20 MB/s: the link isn't full.
	rep = report(10*time.Second, 10, 200e6, ctx, map[Phase]time.Duration{Data: 8 * time.Second, DstCreate: time.Second})
	if has(rep.Hints, "USB link") {
		t.Errorf("should not fire at half the link speed: %q", rep.Hints)
	}
	// Unknown link: never.
	rep = report(10*time.Second, 10, 350e6, Context{}, map[Phase]time.Duration{Data: 8 * time.Second})
	if has(rep.Hints, "USB link") {
		t.Errorf("should not fire without a known link: %q", rep.Hints)
	}
}

func TestHintPerFileOverhead(t *testing.T) {
	ctx := Context{Destination: `D:\Backup`}
	phases := map[Phase]time.Duration{DstCreate: 30 * time.Second, Close: 10 * time.Second, Rename: 5 * time.Second, Data: 40 * time.Second}
	rep := report(time.Minute, 1000, 1000*100<<10, ctx, phases) // avg 100 KB
	if !has(rep.Hints, "Creating files is the slow part") || !has(rep.Hints, `D:\Backup`) || !has(rep.Hints, "never changes") {
		t.Errorf("should fire: %q", rep.Hints)
	}
	rep = report(time.Minute, 1000, 1000*(1<<20), ctx, phases) // avg 1 MB: big files
	if has(rep.Hints, "Creating files") {
		t.Errorf("should not fire for big files: %q", rep.Hints)
	}
	rep = report(time.Minute, 1000, 1000*100<<10, ctx, map[Phase]time.Duration{DstCreate: 5 * time.Second, Data: 40 * time.Second})
	if has(rep.Hints, "Creating files") {
		t.Errorf("should not fire when creating is cheap: %q", rep.Hints)
	}
}

func TestHintListing(t *testing.T) {
	rep := report(100*time.Second, 10, 10, Context{PlanSource: "walk"}, map[Phase]time.Duration{PlanList: 41 * time.Second})
	if !has(rep.Hints, "Listing took 41.0 s") {
		t.Errorf("should fire: %q", rep.Hints)
	}
	rep = report(100*time.Second, 10, 10, Context{PlanSource: "phone-find"}, map[Phase]time.Duration{PhoneList: 30 * time.Second})
	if !has(rep.Hints, "Listing the phone took") {
		t.Errorf("should fire for phones: %q", rep.Hints)
	}
	rep = report(100*time.Second, 10, 10, Context{}, map[Phase]time.Duration{PlanList: 10 * time.Second})
	if has(rep.Hints, "Listing") {
		t.Errorf("should not fire at 10%%: %q", rep.Hints)
	}
}

func TestHintPhoneRoundTrips(t *testing.T) {
	ctx := Context{PhoneMode: "pull", LinkMBps: 40}
	rep := report(100*time.Second, 5000, 5000*200<<10, ctx, map[Phase]time.Duration{PhoneStream: 90 * time.Second}) // ~10 MB/s
	if !has(rep.Hints, "tar mode helps") {
		t.Errorf("should fire: %q", rep.Hints)
	}
	ctx.PhoneMode = "tar"
	rep = report(100*time.Second, 5000, 5000*200<<10, ctx, nil)
	if has(rep.Hints, "tar mode") {
		t.Errorf("should not fire in tar mode: %q", rep.Hints)
	}
	ctx.PhoneMode = "pull"
	rep = report(100*time.Second, 50, 50*5<<20, ctx, nil) // big files
	if has(rep.Hints, "tar mode") {
		t.Errorf("should not fire for big files: %q", rep.Hints)
	}
}

func TestHintSeekBound(t *testing.T) {
	ctx := Context{SrcKind: "usb-hdd", DriveBenchMBps: 120}
	rep := report(100*time.Second, 10000, 1000e6, ctx, nil) // 10 MB/s of 120
	if !has(rep.Hints, "hard disk is seeking") {
		t.Errorf("should fire: %q", rep.Hints)
	}
	rep = report(100*time.Second, 10000, 8000e6, ctx, nil) // 80 MB/s
	if has(rep.Hints, "seeking") {
		t.Errorf("should not fire at 80 MB/s: %q", rep.Hints)
	}
	rep = report(100*time.Second, 10000, 1000e6, Context{SrcKind: "usb-flash"}, nil)
	if has(rep.Hints, "seeking") {
		t.Errorf("should not fire for flash: %q", rep.Hints)
	}
}

func TestAtMostThreeHintsMostImportantFirst(t *testing.T) {
	ctx := Context{LinkMBps: 40, PhoneMode: "pull", SrcKind: "hdd", PlanSource: "phone-find"}
	phases := map[Phase]time.Duration{
		Data: 75 * time.Second, DstCreate: 30 * time.Second, Close: 20 * time.Second,
		PhoneList: 50 * time.Second,
	}
	rep := report(100*time.Second, 5000, 3500e6, ctx, phases)
	if len(rep.Hints) > MaxHints {
		t.Fatalf("%d hints: %q", len(rep.Hints), rep.Hints)
	}
}

// BenchmarkRecorderNil and BenchmarkRecorderOn show the per-call cost.
func BenchmarkRecorderNil(b *testing.B) {
	var r *Recorder
	for i := 0; i < b.N; i++ {
		r.End(Data, r.Start())
	}
}

func BenchmarkRecorderOn(b *testing.B) {
	r := New()
	for i := 0; i < b.N; i++ {
		r.End(Data, r.Start())
	}
}
