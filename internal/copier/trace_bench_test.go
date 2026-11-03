package copier

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
)

// smallFilesSource makes n small files (1-4 KB) for the overhead benchmarks.
func smallFilesSource(b *testing.B, n int) string {
	b.Helper()
	src := b.TempDir()
	data := make([]byte, 4096)
	for i := 0; i < n; i++ {
		dir := filepath.Join(src, "d"+string(rune('a'+i%8)))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "f"+itoa(i)+".txt"), data[:1024+i%3072], 0o644)
	}
	return src
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func benchCopySmallFiles(b *testing.B, tr func() *trace.Recorder) {
	src := smallFilesSource(b, 500)
	dst := filepath.Join(b.TempDir(), "out")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		os.RemoveAll(dst) // every round copies everything (nothing is "already there")
		b.StartTimer()
		p, err := PlanLocal(context.Background(), src, dst, OnConflictNewer, 4)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := Run(context.Background(), p, Options{Workers: 4, RetryDelay: time.Millisecond, Trace: tr()}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCopySmallFilesNoTrace and BenchmarkCopySmallFilesTrace compare a
// copy of 500 small files without and with the tracer. BUILD-V2 stage 13: the
// difference must stay under 2% (the tracer is a few atomic adds per file).
func BenchmarkCopySmallFilesNoTrace(b *testing.B) {
	benchCopySmallFiles(b, func() *trace.Recorder { return nil })
}

func BenchmarkCopySmallFilesTrace(b *testing.B) {
	benchCopySmallFiles(b, trace.New)
}

func TestTracedCopyRecordsEveryPhase(t *testing.T) {
	src, res := source(t, 60)
	dst := filepath.Join(t.TempDir(), "out")
	tr := trace.New()
	ctx := trace.WithRecorder(context.Background(), tr)
	p, err := PlanLocal(ctx, src, dst, OnConflictNewer, 4)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "walk" {
		t.Errorf("plan source %q", p.Source)
	}
	r := mustRun(t, p, Options{Workers: 4, Verify: true, RetryDelay: time.Millisecond, Trace: tr})
	rep := tr.Report(r.Elapsed, trace.Context{Workers: 4, PlanSource: p.Source})
	if rep.Files != int64(res.Files) || rep.Bytes != r.BytesWritten {
		t.Errorf("trace counted %d files / %d bytes, copy wrote %d / %d", rep.Files, rep.Bytes, res.Files, r.BytesWritten)
	}
	for _, ph := range []trace.Phase{trace.PlanList, trace.PlanDst, trace.SrcOpen, trace.DstCreate, trace.Data, trace.SetTime, trace.Close, trace.Rename, trace.Verify} {
		if rep.Time(ph) <= 0 {
			t.Errorf("phase %q was not recorded", ph)
		}
	}
}
