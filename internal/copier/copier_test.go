package copier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/fixture"
	"github.com/yashwardhanv/dd/internal/ui"
)

// fastOpts keeps retries quick in tests.
func fastOpts(workers int) Options { return Options{Workers: workers, RetryDelay: time.Millisecond} }

// source makes a fixture tree to copy from and returns its path and what it holds.
func source(t *testing.T, files int) (string, fixture.Result) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Source Tree")
	res, err := fixture.Generate(root, fixture.Options{Files: files, Depth: 3, Fanout: 4, Seed: 5})
	if err != nil {
		t.Fatal(err)
	}
	return root, res
}

// listTree returns "relative path" -> file size for every FILE under root, and the list of folders.
func listTree(t *testing.T, root string) (files map[string]int64, dirs []string) {
	t.Helper()
	files = map[string]int64{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		info, _ := d.Info()
		files[rel] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(dirs)
	return files, dirs
}

func plan(t *testing.T, src, dst, policy string) *Plan {
	t.Helper()
	p, err := PlanLocal(context.Background(), src, dst, policy, 4)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRun(t *testing.T, p *Plan, opt Options) Result {
	t.Helper()
	res, err := Run(context.Background(), p, opt)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---- decisions ---------------------------------------------------------------------

type fakeInfo struct {
	size  int64
	mtime int64
	dir   bool
}

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeInfo) ModTime() time.Time { return time.Unix(f.mtime, 0) }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

func TestDecide(t *testing.T) {
	const T = 1_700_000_000
	cases := []struct {
		name   string
		policy string
		size   int64
		mtime  int64
		dst    fs.FileInfo
		want   Decision
	}{
		{"missing -> copy", "newer", 10, T, nil, Copy},
		{"same size and time -> skip (resume)", "newer", 10, T, fakeInfo{10, T, false}, SkipSame},
		{"FAT 2-second rounding is still the same", "overwrite", 10, T, fakeInfo{10, T + 2, false}, SkipSame},
		{"3 seconds apart is NOT the same (newer: src older -> keep)", "newer", 10, T, fakeInfo{10, T + 3, false}, SkipNewer},
		{"newer: source newer -> overwrite", "newer", 10, T + 100, fakeInfo{10, T, false}, Overwrite},
		{"newer: different size, source newer -> overwrite", "newer", 99, T + 100, fakeInfo{10, T, false}, Overwrite},
		{"newer: destination newer -> keep", "newer", 10, T, fakeInfo{99, T + 100, false}, SkipNewer},
		{"skip: always leave it", "skip", 10, T + 100, fakeInfo{99, T, false}, SkipPolicy},
		{"overwrite: always replace", "overwrite", 10, T, fakeInfo{99, T + 100, false}, Overwrite},
		{"default policy is newer", "", 10, T + 5, fakeInfo{99, T, false}, Overwrite},
	}
	for _, c := range cases {
		got, blocked := decide(c.policy, c.size, c.mtime, c.dst)
		if got != c.want || blocked != "" {
			t.Errorf("%s: got %v (%q), want %v", c.name, got, blocked, c.want)
		}
	}
	if _, blocked := decide("newer", 1, T, fakeInfo{dir: true}); blocked == "" {
		t.Error("a folder where a file should go must be reported")
	}
	if !SkipSame.Skipped() || Copy.Skipped() || Overwrite.Skipped() {
		t.Error("Skipped() wrong")
	}
	for _, d := range []Decision{Copy, Overwrite, SkipSame, SkipNewer, SkipPolicy} {
		if d.String() == "?" || d.String() == "" {
			t.Errorf("Decision %d has no description", d)
		}
	}
	if !ValidPolicy("newer") || !ValidPolicy("skip") || !ValidPolicy("overwrite") || ValidPolicy("merge") || ValidPolicy("") {
		t.Error("ValidPolicy wrong")
	}
}

// ---- planning ------------------------------------------------------------------------

func TestPlanLocalFolderGoesIntoDestinationLikeExplorer(t *testing.T) {
	src, res := source(t, 120)
	dst := t.TempDir()
	p := plan(t, src, dst, "")

	c := p.Counts()
	if int(c.Files) != res.Files || int(c.Copy) != res.Files || c.Skipped != 0 {
		t.Errorf("counts = %+v, fixture = %+v", c, res)
	}
	for _, it := range p.Items {
		if !strings.HasPrefix(it.Rel, "Source Tree"+string(filepath.Separator)) {
			t.Fatalf("%q must be inside dst\\<source folder name>\\", it.Rel)
		}
	}
	// Folders include the top folder and every empty folder.
	if len(p.Dirs) != res.Dirs+1 {
		t.Errorf("planned %d folders, want %d (+1 for the top folder)", len(p.Dirs), res.Dirs)
	}
	if p.OnConflict != OnConflictNewer {
		t.Errorf("default policy = %q", p.OnConflict)
	}
}

func TestPlanSortsLargestFirst(t *testing.T) {
	src, _ := source(t, 200)
	p := plan(t, src, t.TempDir(), "")
	for i := 1; i < len(p.Items); i++ {
		if p.Items[i-1].Size < p.Items[i].Size {
			t.Fatalf("item %d (%d bytes) is bigger than the one before it (%d)", i, p.Items[i].Size, p.Items[i-1].Size)
		}
	}
}

func TestPlanSingleFileLandsDirectlyInDestination(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "one.txt")
	os.WriteFile(f, []byte("hello"), 0o644)
	p := plan(t, f, filepath.Join(t.TempDir(), "out"), "")
	if len(p.Items) != 1 || p.Items[0].Rel != "one.txt" || p.Items[0].Size != 5 || len(p.Dirs) != 0 {
		t.Errorf("plan = %+v", p)
	}
	res := mustRun(t, p, fastOpts(2))
	if got, _ := os.ReadFile(filepath.Join(p.DstRoot, "one.txt")); string(got) != "hello" || res.Written != 1 {
		t.Errorf("copied %q, result %+v", got, res)
	}
}

func TestPlanDecisionsAgainstExistingDestination(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(48 * time.Hour)
	write := func(root, name, body string, mt time.Time) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, mt, mt)
	}
	now := time.Now().Truncate(time.Second)
	base := filepath.Base(src)
	write(src, "same.txt", "aaaa", now)
	write(src, "changed-newer-src.txt", "new content", now)
	write(src, "changed-newer-dst.txt", "src", now)
	write(src, "fresh.txt", "x", now)
	write(dst+"/"+base, "same.txt", "aaaa", now)
	write(dst+"/"+base, "changed-newer-src.txt", "old", old)
	write(dst+"/"+base, "changed-newer-dst.txt", "dst content differs", newer)

	byName := func(p *Plan) map[string]Decision {
		m := map[string]Decision{}
		for _, it := range p.Items {
			m[filepath.Base(it.Rel)] = it.Decision
		}
		return m
	}
	want := map[string]map[string]Decision{
		"newer":     {"same.txt": SkipSame, "changed-newer-src.txt": Overwrite, "changed-newer-dst.txt": SkipNewer, "fresh.txt": Copy},
		"skip":      {"same.txt": SkipSame, "changed-newer-src.txt": SkipPolicy, "changed-newer-dst.txt": SkipPolicy, "fresh.txt": Copy},
		"overwrite": {"same.txt": SkipSame, "changed-newer-src.txt": Overwrite, "changed-newer-dst.txt": Overwrite, "fresh.txt": Copy},
	}
	for policy, w := range want {
		got := byName(plan(t, src, dst, policy))
		for name, d := range w {
			if got[name] != d {
				t.Errorf("policy %s, %s: got %v, want %v", policy, name, got[name], d)
			}
		}
	}
	c := plan(t, src, dst, "newer").Counts()
	if c.Files != 4 || c.Copy != 1 || c.Overwrite != 1 || c.Skipped != 2 {
		t.Errorf("counts = %+v", c)
	}
	if c.WriteBytes != int64(len("new content")+len("x")) {
		t.Errorf("WriteBytes = %d: only files that will be written count", c.WriteBytes)
	}
}

// ---- running -------------------------------------------------------------------------

func TestCopyReproducesTheTreeWithSizesAndTimes(t *testing.T) {
	src, res := source(t, 300)
	dst := filepath.Join(t.TempDir(), "backup") // doesn't exist yet: created on demand
	p := plan(t, src, dst, "")

	r := mustRun(t, p, fastOpts(4))
	if int(r.Written) != res.Files || r.Failed != 0 || r.Skipped != 0 || r.Cancelled {
		t.Fatalf("result = %+v (fixture %+v)", r, res)
	}

	srcFiles, srcDirs := listTree(t, src)
	gotFiles, gotDirs := listTree(t, filepath.Join(dst, "Source Tree"))
	if len(gotFiles) != len(srcFiles) {
		t.Fatalf("copied %d files, source has %d", len(gotFiles), len(srcFiles))
	}
	for rel, size := range srcFiles {
		if gotFiles[rel] != size {
			t.Errorf("%s: size %d, want %d", rel, gotFiles[rel], size)
		}
		a, _ := os.Stat(filepath.Join(src, rel))
		b, _ := os.Stat(filepath.Join(dst, "Source Tree", rel))
		if a.ModTime().Unix() != b.ModTime().Unix() {
			t.Errorf("%s: mtime %v, want %v", rel, b.ModTime(), a.ModTime())
		}
	}
	if strings.Join(gotDirs, "|") != strings.Join(srcDirs, "|") {
		t.Errorf("folders differ (empty folders must be copied too):\n got %q\nwant %q", gotDirs, srcDirs)
	}
	// Contents, not just sizes (spot check).
	for rel := range srcFiles {
		a, _ := os.ReadFile(filepath.Join(src, rel))
		b, _ := os.ReadFile(filepath.Join(dst, "Source Tree", rel))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: contents differ", rel)
		}
	}
	leftovers(t, dst)
}

// leftovers fails the test if any .ddxpart or staging file remains.
func leftovers(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
		if strings.HasSuffix(p, PartSuffix) || strings.Contains(filepath.Base(p), stagingPrefix) {
			t.Errorf("leftover temporary file: %s", p)
		}
		return nil
	})
}

func TestRunningAgainSkipsEverything(t *testing.T) {
	src, res := source(t, 150)
	dst := t.TempDir()
	mustRun(t, plan(t, src, dst, ""), fastOpts(4))

	p := plan(t, src, dst, "")
	c := p.Counts()
	if int(c.Skipped) != res.Files || c.Copy+c.Overwrite != 0 {
		t.Fatalf("second plan = %+v: everything should be skipped as already there", c)
	}
	var writes atomic.Int64
	openSource = func(string) (*os.File, error) { writes.Add(1); return nil, errors.New("must not be opened") }
	defer func() { openSource = os.Open }()
	r := mustRun(t, p, fastOpts(4))
	if r.Written != 0 || int(r.Skipped) != res.Files || writes.Load() != 0 {
		t.Errorf("result = %+v, source opened %d times", r, writes.Load())
	}
}

// BUILD.md: cancel midway, rerun -> the copy completes and no .ddxpart files remain.
func TestCancelMidwayThenResume(t *testing.T) {
	src, res := source(t, 400)
	dst := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	opt := fastOpts(3)
	opt.Progress = func(p ui.CopyProgress) {} // progress reporter running during cancel is part of the scenario
	// Cancel after about 100 files have been opened.
	openSource = func(name string) (*os.File, error) {
		if seen.Add(1) == 100 {
			cancel()
		}
		return os.Open(name)
	}
	p := plan(t, src, dst, "")
	r, err := Run(ctx, p, opt)
	openSource = os.Open
	if err != nil {
		t.Fatal(err)
	}
	if !r.Cancelled || int(r.Written) >= res.Files || r.Failed != 0 {
		t.Fatalf("expected a partial, cancelled copy without failures: %+v", r)
	}
	leftovers(t, dst) // cancelled copies clean up their partial files too

	// Plant a stale partial file, like a crash would leave.
	stale := filepath.Join(dst, "Source Tree", "crashed.bin"+PartSuffix)
	os.WriteFile(stale, []byte("partial"), 0o644)

	// Run the same command again: it resumes.
	p2 := plan(t, src, dst, "")
	c := p2.Counts()
	if c.Skipped == 0 || c.Copy == 0 {
		t.Errorf("resume plan should mix skipped (%d) and remaining (%d)", c.Skipped, c.Copy)
	}
	r2 := mustRun(t, p2, fastOpts(3))
	if r2.Failed != 0 || r2.Cancelled || int(r2.Written+r2.Skipped) != res.Files {
		t.Fatalf("resume result = %+v", r2)
	}
	os.Remove(stale) // (a stale file for a name that isn't in the source is simply ignored; remove it for the check)
	leftovers(t, dst)

	srcFiles, _ := listTree(t, src)
	gotFiles, _ := listTree(t, filepath.Join(dst, "Source Tree"))
	if len(gotFiles) != len(srcFiles) {
		t.Errorf("after resume: %d files, want %d", len(gotFiles), len(srcFiles))
	}
	for rel, size := range srcFiles {
		if gotFiles[rel] != size {
			t.Errorf("%s: %d bytes, want %d", rel, gotFiles[rel], size)
		}
	}
}

func TestStalePartFileOfARealTargetIsOverwritten(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("the real content"), 0o644)
	dst := t.TempDir()
	base := filepath.Base(src)
	os.MkdirAll(filepath.Join(dst, base), 0o755)
	os.WriteFile(filepath.Join(dst, base, "a.txt"+PartSuffix), []byte("garbage from a crash"), 0o644)

	mustRun(t, plan(t, src, dst, ""), fastOpts(1))
	got, _ := os.ReadFile(filepath.Join(dst, base, "a.txt"))
	if string(got) != "the real content" {
		t.Errorf("got %q", got)
	}
	leftovers(t, dst)
}

func TestRealNameNeverHoldsAHalfWrittenFile(t *testing.T) {
	src := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB: several buffers
	os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644)
	dst := t.TempDir()
	final := filepath.Join(dst, filepath.Base(src), "big.bin")

	ctx, cancel := context.WithCancel(context.Background())
	openSource = func(name string) (*os.File, error) {
		f, err := os.Open(name)
		cancel() // cancelled right as copying starts
		return f, err
	}
	defer func() { openSource = os.Open }()
	Run(ctx, plan(t, src, dst, ""), fastOpts(1))
	if _, err := os.Stat(final); err == nil {
		t.Error("a cancelled copy must not leave anything under the real name")
	}
	leftovers(t, dst)
}

func TestConflictPoliciesWhenRunning(t *testing.T) {
	setup := func() (src, dst string) {
		src, dst = t.TempDir(), t.TempDir()
		base := filepath.Base(src)
		os.MkdirAll(filepath.Join(dst, base), 0o755)
		os.WriteFile(filepath.Join(src, "doc.txt"), []byte("SOURCE VERSION"), 0o644)
		return
	}
	dstFile := func(src, dst string) string { return filepath.Join(dst, filepath.Base(src), "doc.txt") }

	cases := []struct {
		policy  string
		dstAge  time.Duration // destination mtime relative to now (source is "now")
		wantNew bool          // is the destination replaced by the source version?
	}{
		{"newer", -time.Hour, true},
		{"newer", time.Hour, false},
		{"skip", -time.Hour, false},
		{"overwrite", time.Hour, true},
		{"overwrite", -time.Hour, true},
	}
	for _, c := range cases {
		src, dst := setup()
		df := dstFile(src, dst)
		os.WriteFile(df, []byte("old dst"), 0o644)
		mt := time.Now().Add(c.dstAge)
		os.Chtimes(df, mt, mt)
		mustRun(t, plan(t, src, dst, c.policy), fastOpts(1))
		got, _ := os.ReadFile(df)
		if replaced := string(got) == "SOURCE VERSION"; replaced != c.wantNew {
			t.Errorf("%s with destination %v old: replaced=%v want %v", c.policy, c.dstAge, replaced, c.wantNew)
		}
	}
}

func TestDestinationThatIsAFolderIsAFailureNotACrash(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "x.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(src, "y.txt"), []byte("2"), 0o644)
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, filepath.Base(src), "x.txt"), 0o755) // a FOLDER named x.txt

	r := mustRun(t, plan(t, src, dst, ""), fastOpts(2))
	if r.Failed != 1 || r.Written != 1 || len(r.Failures) != 1 || !strings.Contains(r.Failures[0].Err, "folder") {
		t.Errorf("result = %+v", r)
	}
}

func TestRetriesThenReportsFailureAndCarriesOn(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"good1.txt", "bad.txt", "good2.txt"} {
		os.WriteFile(filepath.Join(src, n), []byte("data "+n), 0o644)
	}
	dst := t.TempDir()

	var badAttempts atomic.Int64
	openSource = func(name string) (*os.File, error) {
		if filepath.Base(name) == "bad.txt" {
			badAttempts.Add(1)
			return nil, errors.New("simulated read error")
		}
		return os.Open(name)
	}
	defer func() { openSource = os.Open }()

	r := mustRun(t, plan(t, src, dst, ""), fastOpts(2))
	if r.Written != 2 || r.Failed != 1 {
		t.Fatalf("result = %+v", r)
	}
	if badAttempts.Load() != 3 { // 1 try + 2 retries
		t.Errorf("bad.txt attempted %d times, want 3", badAttempts.Load())
	}
	if len(r.Failures) != 1 || r.Failures[0].Rel != filepath.Join(filepath.Base(src), "bad.txt") || !strings.Contains(r.Failures[0].Err, "simulated read error") {
		t.Errorf("failures = %+v", r.Failures)
	}
	leftovers(t, dst)
}

func TestARetrySucceedsWhenTheProblemIsTemporary(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "flaky.txt"), []byte("eventually"), 0o644)
	dst := t.TempDir()
	var tries atomic.Int64
	openSource = func(name string) (*os.File, error) {
		if tries.Add(1) < 3 {
			return nil, errors.New("temporarily unavailable")
		}
		return os.Open(name)
	}
	defer func() { openSource = os.Open }()
	r := mustRun(t, plan(t, src, dst, ""), fastOpts(1))
	if r.Written != 1 || r.Failed != 0 || tries.Load() != 3 {
		t.Errorf("result = %+v after %d tries", r, tries.Load())
	}
}

func TestOnlyFirst20FailuresAreKept(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 30; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644)
	}
	openSource = func(string) (*os.File, error) { return nil, errors.New("nope") }
	defer func() { openSource = os.Open }()
	r := mustRun(t, plan(t, src, t.TempDir(), ""), fastOpts(4))
	if r.Failed != 30 || len(r.Failures) != 20 {
		t.Errorf("failed=%d kept=%d, want 30/20", r.Failed, len(r.Failures))
	}
}

func TestSymlinksAreSkippedNotFollowed(t *testing.T) {
	src := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside the source"), 0o644)
	os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("can't create symlinks here: %v", err)
	}
	dst := t.TempDir()
	r := mustRun(t, plan(t, src, dst, ""), fastOpts(2))
	if r.Written != 1 || r.NotRegular != 1 || r.Failed != 0 {
		t.Errorf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dst, filepath.Base(src), "link.txt")); err == nil {
		t.Error("a symlink's target must never be copied")
	}
}

func TestVerifyDetectsACorruptCopyAndRemovesIt(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.bin"), bytes.Repeat([]byte{7}, 5000), 0o644)
	os.WriteFile(filepath.Join(src, "b.bin"), bytes.Repeat([]byte{9}, 5000), 0o644)
	dst := t.TempDir()

	// Corrupt a.bin's copy (flip a byte) after it is written - the verify step must notice.
	afterCopy = func(d string) {
		if filepath.Base(d) == "a.bin" {
			data, _ := os.ReadFile(d)
			data[100] ^= 0xFF
			os.WriteFile(d, data, 0o644)
		}
	}
	defer func() { afterCopy = nil }()

	opt := fastOpts(1)
	opt.Verify = true
	r := mustRun(t, plan(t, src, dst, ""), opt)
	if r.Failed != 1 || r.VerifyMismatches < 1 || r.Written != 1 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Failures[0].Err, "verification failed") {
		t.Errorf("failure = %+v", r.Failures[0])
	}
	if _, err := os.Stat(filepath.Join(dst, filepath.Base(src), "a.bin")); err == nil {
		t.Error("the corrupt copy must be deleted so a re-run copies it again instead of treating it as done")
	}
}

func TestVerifyPassesForGoodCopies(t *testing.T) {
	src, res := source(t, 120)
	opt := fastOpts(4)
	opt.Verify = true
	r := mustRun(t, plan(t, src, t.TempDir(), ""), opt)
	if int(r.Written) != res.Files || r.Failed != 0 || r.VerifyMismatches != 0 {
		t.Errorf("result = %+v", r)
	}
}

func TestProgressReportsAreMonotonicAndEndComplete(t *testing.T) {
	src, res := source(t, 200)
	// Slow the copy a little so the 200 ms reporter has a chance to fire.
	openSource = func(name string) (*os.File, error) { time.Sleep(3 * time.Millisecond); return os.Open(name) }
	defer func() { openSource = os.Open }()

	var snaps []ui.CopyProgress
	opt := fastOpts(2)
	opt.Progress = func(p ui.CopyProgress) { snaps = append(snaps, p) }
	r := mustRun(t, plan(t, src, t.TempDir(), ""), opt)

	if len(snaps) < 2 {
		t.Fatalf("expected several progress reports, got %d", len(snaps))
	}
	for i := 1; i < len(snaps); i++ {
		if snaps[i].FilesDone < snaps[i-1].FilesDone || snaps[i].BytesDone < snaps[i-1].BytesDone {
			t.Fatalf("progress went backwards: %+v then %+v", snaps[i-1], snaps[i])
		}
	}
	last := snaps[len(snaps)-1]
	if int(last.FilesDone) != res.Files || int(last.FilesTotal) != res.Files || last.BytesDone != last.BytesTotal || last.BytesDone != r.BytesWritten {
		t.Errorf("final snapshot = %+v, result %+v", last, r)
	}
}

func TestDestinationCreationFailureIsFatal(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o644)
	blocker := filepath.Join(t.TempDir(), "iam-a-file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	p := plan(t, src, filepath.Join(blocker, "sub"), "") // can't create a folder inside a file
	if _, err := Run(context.Background(), p, fastOpts(1)); err == nil {
		t.Error("expected an error when the destination can't be created")
	}
}

// ---- pre-checks ------------------------------------------------------------------------

func fakePlan(dst string, sizes ...int64) *Plan {
	var jobs []Job
	for i, s := range sizes {
		jobs = append(jobs, Job{Src: fmt.Sprintf("/x/%d", i), Rel: fmt.Sprintf("f%d.bin", i), Size: s})
	}
	return PlanJobs(dst, jobs, "", 1)
}

func TestFAT32RefusesFilesOfFourGiBOrMore(t *testing.T) {
	const GiB = int64(1 << 30)
	fatChecks := func(name string) Checks {
		return Checks{FreeBytes: func(string) (uint64, error) { return 1 << 60, nil }, FileSystem: func(string) (string, error) { return name, nil }}
	}
	dst := t.TempDir()

	if err := fakePlan(dst, GiB, 4*GiB).Check(fatChecks("FAT32")); !errors.Is(err, ErrFileTooBigForFAT32) || !strings.Contains(err.Error(), "f1.bin") {
		t.Errorf("4 GiB file on FAT32: %v", err)
	}
	if err := fakePlan(dst, 5*GiB).Check(fatChecks("fat32")); !errors.Is(err, ErrFileTooBigForFAT32) {
		t.Errorf("case-insensitive FS name: %v", err)
	}
	if err := fakePlan(dst, 4*GiB-1, GiB).Check(fatChecks("FAT32")); err != nil {
		t.Errorf("4 GiB minus 1 byte fits FAT32: %v", err)
	}
	for _, fsName := range []string{"exFAT", "NTFS"} {
		if err := fakePlan(dst, 40*GiB).Check(fatChecks(fsName)); err != nil {
			t.Errorf("%s has no 4 GiB limit: %v", fsName, err)
		}
	}
	// A huge file that is being SKIPPED doesn't matter.
	p := fakePlan(dst, 5*GiB)
	p.Items[0].Decision = SkipSame
	if err := p.Check(fatChecks("FAT32")); err != nil {
		t.Errorf("skipped file: %v", err)
	}
	// Unknown file system: no refusal.
	unknown := Checks{FileSystem: func(string) (string, error) { return "", errors.ErrUnsupported }}
	if err := fakePlan(dst, 5*GiB).Check(unknown); err != nil {
		t.Errorf("unknown FS: %v", err)
	}
}

func TestFreeSpaceCheck(t *testing.T) {
	dst := t.TempDir()
	free := func(n uint64) Checks { return Checks{FreeBytes: func(string) (uint64, error) { return n, nil }} }

	if err := fakePlan(dst, 600, 500).Check(free(1000)); !errors.Is(err, ErrNotEnoughSpace) {
		t.Errorf("1100 bytes into 1000 free: %v", err)
	}
	if err := fakePlan(dst, 600, 400).Check(free(1000)); err != nil {
		t.Errorf("exactly fits: %v", err)
	}
	// Skipped files don't need space.
	p := fakePlan(dst, 600, 900)
	p.Items[0].Decision = SkipSame // the 900-byte one (sorted first)
	if err := p.Check(free(700)); err != nil {
		t.Errorf("only the 600-byte file needs space: %v", err)
	}
	// A failing query doesn't block the copy.
	broken := Checks{FreeBytes: func(string) (uint64, error) { return 0, errors.New("can't tell") }}
	if err := fakePlan(dst, 1<<40).Check(broken); err != nil {
		t.Errorf("unknown free space: %v", err)
	}
	// Nothing to write: no checks at all.
	if err := fakePlan(dst).Check(free(0)); err != nil {
		t.Errorf("empty plan: %v", err)
	}
	// The real system query works on an existing folder (and for a not-yet-created destination).
	if v, err := diskFree(t.TempDir()); err != nil || v == 0 {
		t.Errorf("diskFree = %d, %v", v, err)
	}
	if p := nearestExisting(filepath.Join(dst, "a", "b", "c")); p != dst {
		t.Errorf("nearestExisting = %q, want %q", p, dst)
	}
}

func TestAutoWorkers(t *testing.T) {
	cases := []struct {
		srcP, srcK, dstP, dstK bool
		want                   int
	}{
		{true, true, false, true, 2},   // source is an HDD
		{false, true, true, true, 2},   // destination is an HDD
		{false, true, false, true, 8},  // flash/SSD both ends
		{false, false, false, true, 4}, // can't tell for one end
		{false, false, false, false, 4},
		{true, false, false, false, 4}, // "penalty" is meaningless when unknown
	}
	for _, c := range cases {
		if got := AutoWorkers(c.srcP, c.srcK, c.dstP, c.dstK); got != c.want {
			t.Errorf("%+v: got %d, want %d", c, got, c.want)
		}
	}
}

func TestSrcName(t *testing.T) {
	if got := srcName(filepath.Join("a", "b", "Photos")); got != "Photos" {
		t.Errorf("got %q", got)
	}
	if got := srcName("."); got != "copy" {
		t.Errorf("got %q", got)
	}
	if got := srcName(string(filepath.Separator)); got != "copy" {
		t.Errorf("got %q", got)
	}
}

func TestBufferPoolGivesBigBuffers(t *testing.T) {
	b := bufPool.Get().(*[]byte)
	defer bufPool.Put(b)
	if len(*b) != 1<<20 {
		t.Errorf("buffer is %d bytes, want 1 MB", len(*b))
	}
}
