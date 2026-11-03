package copier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/fixture"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// indexTree indexes root like 'drivedex index <folder>' does and returns the
// scope of sub (a folder inside root, or root itself).
func indexTree(t *testing.T, root, sub string, made time.Time) index.FolderScope {
	t.Helper()
	h := index.FolderHeader(root)
	b := index.NewBuilder(h)
	if err := scan.NewParallel(root, 4).Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	ix := b.Index()
	ix.Header.UpdatedAt = made
	scope, ok := index.LocateScope([]*index.Index{ix}, index.Env{}, sub)
	if !ok {
		t.Fatalf("no scope for %s", sub)
	}
	return scope
}

// sameTree fails unless dst holds exactly src's files (names, sizes, contents).
func sameTree(t *testing.T, src, dst string) {
	t.Helper()
	want, _ := listTree(t, src)
	got, _ := listTree(t, dst)
	for rel, size := range want {
		if got[rel] != size {
			t.Errorf("%s: %d bytes at the destination, %d in the source", rel, got[rel], size)
			continue
		}
		a, _ := os.ReadFile(filepath.Join(src, rel))
		b, _ := os.ReadFile(filepath.Join(dst, rel))
		if string(a) != string(b) {
			t.Errorf("%s: contents differ", rel)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s is at the destination but not in the source", rel)
		}
	}
}

func TestIndexPlanWithSweepCopiesTheCurrentSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Photos")
	if _, err := fixture.Generate(src, fixture.Options{Files: 300, Depth: 3, Fanout: 4, Seed: 17}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := indexTree(t, src, src, now.Add(-2*time.Minute))

	// Change the source after indexing: add 50, change 20, delete 10, rename a folder.
	files, dirs := listTree(t, src)
	var names []string
	for rel := range files {
		names = append(names, rel)
	}
	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("added-%02d.txt", i)), []byte(strings.Repeat("n", i+1)), 0o644)
	}
	changed, deleted := 0, 0
	for _, rel := range names {
		p := filepath.Join(src, rel)
		switch {
		case changed < 20 && strings.Count(rel, string(filepath.Separator)) == 0:
			os.WriteFile(p, []byte("changed after indexing "+rel), 0o644)
			changed++
		case deleted < 10 && strings.Count(rel, string(filepath.Separator)) == 0:
			os.Remove(p)
			deleted++
		}
	}
	if changed < 20 || deleted < 10 {
		// not enough top-level files: take them from anywhere
		for _, rel := range names {
			if changed >= 20 && deleted >= 10 {
				break
			}
			p := filepath.Join(src, rel)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if changed < 20 {
				os.WriteFile(p, []byte("changed "+rel), 0o644)
				changed++
			} else {
				os.Remove(p)
				deleted++
			}
		}
	}
	renamed := ""
	for _, d := range dirs {
		if !strings.Contains(d, string(filepath.Separator)) {
			renamed = d
			break
		}
	}
	if err := os.Rename(filepath.Join(src, renamed), filepath.Join(src, renamed+"-renamed")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "Backup")
	res, err := PlanLocalSmart(context.Background(), LocalPlanRequest{
		Src: src, DstRoot: dst, Policy: OnConflictNewer, Workers: 4, Scope: scope, ScopeOK: true, Mode: "index", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Source != PlanIndex || res.Sweeper == nil || !strings.Contains(res.Why, "Planned from the index") {
		t.Fatalf("source %q why %q", res.Plan.Source, res.Why)
	}
	r := mustRun(t, res.Plan, fastOpts(4))
	if r.Failed != 0 {
		t.Fatalf("failures: %+v", r.Failures)
	}
	sw := res.Sweeper.Result()
	if !sw.Complete || sw.Added < 50 || sw.Modified < 20 || sw.Vanished < 10 {
		t.Errorf("sweep %+v", sw)
	}
	if r.Vanished < 10 || r.Found < 70 {
		t.Errorf("vanished %d (want >= 10 deleted + the renamed folder's files), found %d", r.Vanished, r.Found)
	}
	sameTree(t, src, filepath.Join(dst, "Photos"))
	leftovers(t, dst)

	// A second copy: everything is already there.
	scope = indexTree(t, src, src, time.Now())
	res, _ = PlanLocalSmart(context.Background(), LocalPlanRequest{Src: src, DstRoot: dst, Policy: OnConflictNewer, Workers: 4, Scope: scope, ScopeOK: true, Mode: "auto"})
	r = mustRun(t, res.Plan, fastOpts(4))
	if r.Written != 0 || r.Failed != 0 || r.Found != 0 {
		t.Errorf("second run: %+v", r)
	}
}

func TestIndexPlanFallsBackToWalk(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Docs")
	fixture.Generate(src, fixture.Options{Files: 40, Depth: 2, Fanout: 3, Seed: 2})
	now := time.Now()
	cases := []struct {
		name string
		req  func(r *LocalPlanRequest)
		why  string
	}{
		{"no index", func(r *LocalPlanRequest) { r.ScopeOK = false }, "no saved index"},
		{"old index", func(r *LocalPlanRequest) { r.Scope.Index.Header.UpdatedAt = now.Add(-time.Hour) }, "older than index_plan_max_age"},
		{"hard disk", func(r *LocalPlanRequest) { r.SeekPenalty, r.SeekKnown = true, true }, "hard disk"},
		{"no metadata", func(r *LocalPlanRequest) {
			ix := r.Scope.Index
			for i := range ix.Flags {
				if ix.Flags[i]&index.FlagDir == 0 {
					ix.Flags[i] |= index.FlagNoMeta
				}
			}
		}, "have a size and date"},
		{"--fresh", func(r *LocalPlanRequest) { r.Mode = "walk" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := LocalPlanRequest{Src: src, DstRoot: t.TempDir(), Workers: 2, Scope: indexTree(t, src, src, now), ScopeOK: true, Mode: "auto", Now: now}
			c.req(&r)
			res, err := PlanLocalSmart(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if res.Plan.Source != PlanWalk || res.Sweeper != nil || !strings.Contains(res.Why, c.why) {
				t.Errorf("source %q why %q", res.Plan.Source, res.Why)
			}
			if r.Mode != "walk" {
				r.Mode = "index"
				if _, err := PlanLocalSmart(context.Background(), r); !errors.Is(err, ErrIndexNotUsable) {
					t.Errorf("--plan index: %v", err)
				}
			}
		})
	}
	// "always" (MaxAge < 0) trusts an old index; a journal-refreshed index needs no sweep.
	r := LocalPlanRequest{Src: src, DstRoot: t.TempDir(), Workers: 2, Scope: indexTree(t, src, src, now.Add(-48*time.Hour)), ScopeOK: true, Mode: "auto", MaxAge: -1, Now: now}
	if res, _ := PlanLocalSmart(context.Background(), r); res.Plan.Source != PlanIndex {
		t.Errorf("always: %q", res.Plan.Source)
	}
	r.Refreshed = true
	if res, _ := PlanLocalSmart(context.Background(), r); res.Plan.Source != PlanUSNIndex || res.Sweeper != nil {
		t.Errorf("refreshed: %q sweeper %v", res.Plan.Source, res.Sweeper != nil)
	}
}

func TestIndexPlanOfASubfolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Drive")
	fixture.Generate(root, fixture.Options{Files: 120, Depth: 3, Fanout: 3, Seed: 5})
	_, dirs := listTree(t, root)
	sub := filepath.Join(root, dirs[0])
	scope := indexTree(t, root, sub, time.Now())
	dst := t.TempDir()
	res, err := PlanLocalSmart(context.Background(), LocalPlanRequest{Src: sub, DstRoot: dst, Workers: 4, Scope: scope, ScopeOK: true, Mode: "index"})
	if err != nil {
		t.Fatal(err)
	}
	r := mustRun(t, res.Plan, fastOpts(4))
	if r.Failed != 0 || r.Found != 0 {
		t.Errorf("%+v", r)
	}
	sameTree(t, sub, filepath.Join(dst, filepath.Base(sub)))
}

func TestIndexPlanCancelDuringTheSweepThenResume(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Big")
	fixture.Generate(src, fixture.Options{Files: 600, Depth: 3, Fanout: 4, Seed: 9})
	scope := indexTree(t, src, src, time.Now())
	for i := 0; i < 200; i++ { // added after indexing: only the sweep finds them
		os.WriteFile(filepath.Join(src, fmt.Sprintf("late-%03d.bin", i)), []byte(strings.Repeat("L", 500+i)), 0o644)
	}
	dst := t.TempDir()
	for round := 0; round < 3; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		res, err := PlanLocalSmart(ctx, LocalPlanRequest{Src: src, DstRoot: dst, Workers: 4, Scope: scope, ScopeOK: true, Mode: "index"})
		if err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(time.Duration(2+round*3) * time.Millisecond); cancel() }()
		if _, err := Run(ctx, res.Plan, fastOpts(4)); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	res, _ := PlanLocalSmart(context.Background(), LocalPlanRequest{Src: src, DstRoot: dst, Workers: 4, Scope: scope, ScopeOK: true, Mode: "index"})
	r := mustRun(t, res.Plan, fastOpts(4))
	if r.Failed != 0 {
		t.Fatalf("%+v", r.Failures)
	}
	sameTree(t, src, filepath.Join(dst, "Big"))
	leftovers(t, dst)
}

func TestLateFilesRespectFreeSpace(t *testing.T) {
	src := filepath.Join(t.TempDir(), "S")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("aaaa"), 0o644)
	scope := indexTree(t, src, src, time.Now())
	os.WriteFile(filepath.Join(src, "huge.bin"), make([]byte, 5000), 0o644)
	res, _ := PlanLocalSmart(context.Background(), LocalPlanRequest{Src: src, DstRoot: t.TempDir(), Workers: 2, Scope: scope, ScopeOK: true, Mode: "index"})
	opt := fastOpts(2)
	opt.LateChecks = Checks{FreeBytes: func(string) (uint64, error) { return 1000, nil }}
	r, _ := Run(context.Background(), res.Plan, opt)
	if r.Failed != 1 || !strings.Contains(r.Failures[0].Err, "not enough free space") || r.Written != 1 {
		t.Errorf("%+v", r)
	}
}
