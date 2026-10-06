package copier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPlanLocalRefusesCopyingIntoItself(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	for _, dst := range []string{src, filepath.Join(src, "sub"), filepath.Join(src, "sub", "..", "x")} {
		if _, err := PlanLocal(context.Background(), src, dst, "", 2); !errors.Is(err, ErrDestinationInsideSource) {
			t.Errorf("PlanLocal(%s -> %s) = %v, want ErrDestinationInsideSource", src, dst, err)
		}
	}
	for _, dst := range []string{filepath.Dir(src), src + "2", filepath.Join(filepath.Dir(src), "other")} {
		if _, err := PlanLocal(context.Background(), src, dst, "", 2); err != nil {
			t.Errorf("PlanLocal(%s -> %s) = %v, want it allowed", src, dst, err)
		}
	}
	// A single file may go into the folder it is already in (it is then skipped as "same").
	file := filepath.Join(src, "a.txt")
	p, err := PlanLocal(context.Background(), file, src, "", 1)
	if err != nil || p.Items[0].Decision != SkipSame {
		t.Errorf("file into its own folder: err=%v plan=%+v", err, p)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"plain.jpg":         "plain.jpg",
		"meeting 10:30.txt": "meeting 10_30.txt",
		`a<b>c"d|e?f*g\h`:   "a_b_c_d_e_f_g_h",
		"tab\there":         "tab_here",
		"trailing.":         "trailing_",
		"spaces  ":          "spaces__",
		"CON":               "_CON",
		"con.txt":           "_con.txt",
		"Com1.tar.gz":       "_Com1.tar.gz",
		"lpt9":              "_lpt9",
		"console.txt":       "console.txt",
		"COM10":             "COM10",
		"":                  "_",
		"...":               "___",
		"héllo 😀.jpg":       "héllo 😀.jpg",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got, want := SafeRel(`DCIM\a:b\c?.jpg`), filepath.Join("DCIM", "a_b", "c_.jpg"); got != want {
		t.Errorf("SafeRel = %q, want %q", got, want)
	}
}

// TestStressCancelAndResume copies a tree of awkwardly sized files, cancels the
// copy at random moments several times, and then finishes it. Whatever
// happens, a re-run must end with an exact copy of every file and no
// half-written ".ddxpart" leftovers.
func TestStressCancelAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	rng := rand.New(rand.NewSource(42))
	src := filepath.Join(t.TempDir(), "src")
	sizes := []int{0, 1, bufferSize - 1, bufferSize, bufferSize + 1, 3*bufferSize + 7}
	want := map[string][]byte{}
	for i := 0; i < 1500; i++ {
		rel := filepath.Join(fmt.Sprintf("d%d", i%17), fmt.Sprintf("s%d", i%5), fmt.Sprintf("f%04d.bin", i))
		n := rng.Intn(4096)
		if i%50 == 0 {
			n = sizes[(i/50)%len(sizes)]
		}
		data := make([]byte, n)
		rng.Read(data)
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		want[filepath.Join("src", rel)] = data
	}
	dst := filepath.Join(t.TempDir(), "dst")

	cancelled := 0
	for round := 0; round < 4; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		stopAt := int64(50 + rng.Intn(300))
		p, err := PlanLocal(ctx, src, dst, OnConflictNewer, 8)
		if err != nil {
			t.Fatal(err)
		}
		var done atomic.Int64
		afterCopy = func(string) { // runs after each file lands
			if done.Add(1) == stopAt {
				cancel()
			}
		}
		res, err := Run(ctx, p, Options{Workers: 8, Verify: round%2 == 0})
		cancel()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if res.Failed != 0 || res.VerifyMismatches != 0 {
			t.Fatalf("round %d: %d failed, %d mismatches: %+v", round, res.Failed, res.VerifyMismatches, res.Failures)
		}
		if res.Cancelled {
			cancelled++
		}
	}
	afterCopy = nil
	if cancelled < 3 {
		t.Fatalf("only %d of 4 rounds were really interrupted; the test isn't testing resume", cancelled)
	}

	// The final, uninterrupted run.
	p, err := PlanLocal(context.Background(), src, dst, OnConflictNewer, 8)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), p, Options{Workers: 8})
	if err != nil || res.Failed != 0 || res.Cancelled {
		t.Fatalf("final run: err=%v res=%+v", err, res)
	}

	got := 0
	filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, PartSuffix) {
			t.Errorf("leftover partial file %s", path)
			return nil
		}
		rel, _ := filepath.Rel(dst, path)
		w, ok := want[rel]
		if !ok {
			t.Errorf("unexpected file %s", rel)
			return nil
		}
		b, _ := os.ReadFile(path)
		if !bytes.Equal(b, w) {
			t.Errorf("%s: content differs (%d vs %d bytes)", rel, len(b), len(w))
		}
		got++
		return nil
	})
	if got != len(want) {
		t.Errorf("copied %d files, want %d", got, len(want))
	}

	// And one more run has nothing left to do.
	p, _ = PlanLocal(context.Background(), src, dst, OnConflictNewer, 8)
	if c := p.Counts(); c.Copy+c.Overwrite != 0 {
		t.Errorf("after a complete copy, a re-run still wants to write %d files", c.Copy+c.Overwrite)
	}
}
