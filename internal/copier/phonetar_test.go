package copier

import (
	"archive/tar"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// checkCopy compares the destination with what the fake phone holds under
// remote (relative names, contents, times) and checks nothing temporary is left.
func checkCopy(t *testing.T, fp *phonetest.FS, files map[string]string, mtimes map[string]int64, dstFolder string) {
	t.Helper()
	got, _ := listTree(t, dstFolder)
	for rel, content := range files {
		p := filepath.Join(dstFolder, filepath.FromSlash(rel))
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if string(data) != content {
			t.Errorf("%s: content differs (%d bytes, want %d)", rel, len(data), len(content))
		}
		if st, _ := os.Stat(p); st != nil && st.ModTime().Unix() != mtimes[rel] {
			t.Errorf("%s: time %d, want %d", rel, st.ModTime().Unix(), mtimes[rel])
		}
	}
	for rel := range got {
		if strings.HasSuffix(rel, PartSuffix) || strings.Contains(rel, stagingPrefix) {
			t.Errorf("left over: %s", rel)
		}
	}
	if len(got) != len(files) {
		t.Errorf("destination holds %d files, want %d", len(got), len(files))
	}
}

func addFiles(fp *phonetest.FS, root string, files map[string]string, mtimes map[string]int64) {
	for rel, c := range files {
		fp.Add(root+"/"+rel, c, mtimes[rel])
	}
}

func TestTarLaneCopiesSmallFilesInOneStream(t *testing.T) {
	fp := phonetest.NewFS()
	files, mtimes := map[string]string{}, map[string]int64{}
	for i := 0; i < 300; i++ {
		rel := fmt.Sprintf("Sub%d/IMG_%04d.jpg", i%3, i)
		files[rel] = strings.Repeat("p", i)
		mtimes[rel] = 1_700_000_000 + int64(i)
	}
	long := strings.Repeat("very-long-folder-name-", 6) + "/" + strings.Repeat("n", 120) + ".jpg" // > 100 chars: GNU/PAX names
	files[long], mtimes[long] = "long", 1_700_000_500
	files["it's a:folder/x.jpg"], mtimes["it's a:folder/x.jpg"] = "quoted", 1_700_000_501 // folder needs SafeName
	addFiles(fp, "/sdcard/DCIM", files, mtimes)

	dst := t.TempDir()
	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM", dst, ""), fastOpts(1))
	if r.PhoneLane != "tar" || r.Written != 302 || r.Failed != 0 || r.StreamRejected != 0 {
		t.Fatalf("result %+v", r)
	}
	if n := len(fp.TarCalls()); n != 1 || fp.PullCount() != 0 {
		t.Errorf("%d tar streams, %d pulls; want 1 and 0", n, fp.PullCount())
	}
	safe := map[string]string{}
	safeT := map[string]int64{}
	for rel, c := range files {
		s := filepath.ToSlash(SafeRel(rel))
		safe[s], safeT[s] = c, mtimes[rel]
	}
	checkCopy(t, fp, safe, safeT, filepath.Join(dst, "DCIM"))
}

func TestTarLaneRefusesWhatWasNotAskedFor(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Add("/sdcard/DCIM/a.jpg", "AAA", 1_700_000_000)
	fp.Add("/sdcard/DCIM/b.jpg", "BBB", 1_700_000_001)
	fp.Add("/sdcard/secret.txt", "not requested", 1_700_000_002)
	fp.TarExtra = func(tw *tar.Writer) {
		for _, h := range []tar.Header{
			{Name: "../evil.txt", Size: 4, Typeflag: tar.TypeReg},
			{Name: "/etc/evil", Size: 4, Typeflag: tar.TypeReg},
			{Name: "link.jpg", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
			{Name: "a.jpg", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, // requested name, but a link
			{Name: "unrequested.jpg", Size: 4, Typeflag: tar.TypeReg},
			{Name: "a.jpg", Size: 4, Typeflag: tar.TypeReg}, // a second copy of a delivered file
			{Name: "dev", Typeflag: tar.TypeChar},
		} {
			h := h
			tw.WriteHeader(&h)
			if h.Size > 0 {
				tw.Write([]byte("evil"))
			}
		}
	}
	base := t.TempDir()
	dst := filepath.Join(base, "dst")
	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM", dst, ""), fastOpts(1))
	if r.Written != 2 || r.Failed != 0 || r.StreamRejected != 7 {
		t.Errorf("result %+v", r)
	}
	checkCopy(t, fp, map[string]string{"a.jpg": "AAA", "b.jpg": "BBB"}, map[string]int64{"a.jpg": 1_700_000_000, "b.jpg": 1_700_000_001}, filepath.Join(dst, "DCIM"))
	for _, p := range []string{filepath.Join(base, "evil.txt"), filepath.Join(dst, "evil.txt"), filepath.Join(dst, "DCIM", "unrequested.jpg"), filepath.Join(dst, "DCIM", "link.jpg")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
}

func TestTarStreamCutMidFile(t *testing.T) {
	files, mtimes := map[string]string{}, map[string]int64{}
	for i := 0; i < 40; i++ {
		rel := fmt.Sprintf("f%02d.bin", i)
		files[rel] = strings.Repeat(string(rune('a'+i%26)), 3000+i)
		mtimes[rel] = 1_700_000_000 + int64(i)
	}
	t.Run("retry with tar", func(t *testing.T) {
		fp := phonetest.NewFS()
		addFiles(fp, "/sdcard/D", files, mtimes)
		fp.TarCut, fp.TarCutTimes = 50_000, 1 // the first stream breaks in the middle of a file
		dst := t.TempDir()
		r := mustRun(t, phonePlan(t, fp, "/sdcard/D", dst, ""), fastOpts(1))
		if r.Failed != 0 || r.Written != 40 || len(fp.TarCalls()) != 2 || fp.PullCount() != 0 {
			t.Errorf("result %+v, %d tar streams, %d pulls", r, len(fp.TarCalls()), fp.PullCount())
		}
		if second := fp.TarCalls()[1]; len(second) >= 40 || len(second) == 0 {
			t.Errorf("the retry should ask only for what didn't arrive, asked for %d", len(second))
		}
		checkCopy(t, fp, files, mtimes, filepath.Join(dst, "D"))
	})
	t.Run("then pull", func(t *testing.T) {
		fp := phonetest.NewFS()
		addFiles(fp, "/sdcard/D", files, mtimes)
		fp.TarCut, fp.TarCutTimes = 50_000, 2 // tar breaks twice: the rest goes through adb pull
		dst := t.TempDir()
		r := mustRun(t, phonePlan(t, fp, "/sdcard/D", dst, ""), fastOpts(1))
		if r.Failed != 0 || r.Written != 40 || fp.PullCount() == 0 {
			t.Errorf("result %+v, %d pulls", r, fp.PullCount())
		}
		checkCopy(t, fp, files, mtimes, filepath.Join(dst, "D"))
	})
}

func TestTarMissingFilesGoToPullThenVanish(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Add("/sdcard/D/keep.jpg", "keep", 1_700_000_000)
	fp.Add("/sdcard/D/gone.jpg", "gone", 1_700_000_000)
	fp.Add("/sdcard/D/locked.jpg", "locked", 1_700_000_000)
	p := phonePlan(t, fp, "/sdcard/D", t.TempDir(), "")
	fp.Remove("/sdcard/D/gone.jpg")              // deleted after planning
	fp.Unreadable["/sdcard/D/locked.jpg"] = true // there, but can't be read
	r := mustRun(t, p, fastOpts(1))
	if r.Written != 1 || r.Vanished != 1 || r.Failed != 1 {
		t.Errorf("result %+v", r)
	}
	if fp.PullCount() == 0 {
		t.Error("missing files must be retried with adb pull")
	}
}

func TestPhoneWithoutTarUsesPull(t *testing.T) {
	fp := phonetest.NewFS()
	fp.NoTar = true
	sampleCamera(fp, 30)
	dst := t.TempDir()
	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if !strings.HasPrefix(r.PhoneLane, "pull (") || r.Written != 30 || len(fp.TarCalls()) != 0 || fp.PullCount() == 0 {
		t.Errorf("lane %q result %+v tars %d", r.PhoneLane, r, len(fp.TarCalls()))
	}
	opt := fastOpts(1)
	opt.PhoneMode = "pull"
	fp2 := phonetest.NewFS()
	sampleCamera(fp2, 5)
	r = mustRun(t, phonePlan(t, fp2, "/sdcard/DCIM/Camera", t.TempDir(), ""), opt)
	if r.PhoneLane != "pull" || len(fp2.TarCalls()) != 0 {
		t.Errorf("--phone-mode pull: %q %d", r.PhoneLane, len(fp2.TarCalls()))
	}
}

func TestTarLaneSplitsBigAndUnsafeFilesToPull(t *testing.T) {
	old := bigFileSize
	bigFileSize = 1000
	defer func() { bigFileSize = old }()
	fp := phonetest.NewFS()
	fp.Features = "sendrecv_v2,sendrecv_v2_lz4"
	fp.Add("/sdcard/D/small.jpg", "s", 1)
	fp.Add("/sdcard/D/movie.mp4", strings.Repeat("m", 2000), 2)
	fp.Add("/sdcard/D/huge.log", strings.Repeat("l", 2000), 3)
	fp.Add("/sdcard/D/a:b.jpg", "colon", 4)
	opt := fastOpts(1)
	opt.PullCompress = "lz4"
	dst := t.TempDir()
	r := mustRun(t, phonePlan(t, fp, "/sdcard/D", dst, ""), opt)
	if r.Written != 4 || r.Failed != 0 {
		t.Fatalf("result %+v", r)
	}
	tars := fp.TarCalls()
	if len(tars) != 1 || strings.Join(tars[0], ",") != "small.jpg" {
		t.Errorf("tar lane got %q", tars)
	}
	var plain, lz4 int
	for _, a := range fp.PullAlgos() {
		if a == "lz4" {
			lz4++
		} else {
			plain++
		}
	}
	if lz4 != 1 || plain != 2 {
		t.Errorf("pulls: %d with lz4 (want 1: huge.log), %d plain (want 2)", lz4, plain)
	}
	if _, err := os.Stat(filepath.Join(dst, "D", "a_b.jpg")); err != nil {
		t.Errorf("unsafe name not saved safely: %v", err)
	}
	// "auto" compression stays off until a benchmark shows it helps.
	fp.Remove("/sdcard/D/small.jpg")
	opt.PullCompress = "auto"
	mustRun(t, phonePlan(t, fp, "/sdcard/D", t.TempDir(), ""), opt)
	for _, a := range fp.PullAlgos()[3:] {
		if a != "" {
			t.Errorf("auto used -z %s", a)
		}
	}
}

func TestTarChunksRespectTheLimits(t *testing.T) {
	var items []*Item
	for i := 0; i < 4500; i++ {
		items = append(items, &Item{Job: Job{Src: fmt.Sprintf("/sdcard/W/f%05d.jpg", i)}})
	}
	chunks := makeTarChunks(items)
	if len(chunks) != 3 || len(chunks[0].rels) != 2000 || len(chunks[2].rels) != 500 || chunks[0].root != "/sdcard/W" {
		t.Errorf("%d chunks: %d %s", len(chunks), len(chunks[0].rels), chunks[0].root)
	}
	items = items[:0]
	for i := 0; i < 300; i++ {
		items = append(items, &Item{Job: Job{Src: "/sdcard/W/" + strings.Repeat("x", 200) + fmt.Sprint(i)}})
	}
	for _, c := range makeTarChunks(items) {
		n := len(c.root)
		for _, r := range c.rels {
			n += len(r) + 3
		}
		if n > maxTarChars+100 {
			t.Errorf("chunk of %d characters", n)
		}
	}
	if got := commonDir([]string{"/sdcard/A/x/1", "/sdcard/A/y/2", "/sdcard/A/3"}); got != "/sdcard/A" {
		t.Errorf("commonDir %q", got)
	}
	if got := commonDir([]string{"/sdcard/A/1", "/storage/B/2"}); got != "/" {
		t.Errorf("commonDir %q", got)
	}
}

// TestTarStressCancelAndResume: 5,000 files through the fake phone, the copy
// stopped at random points 4 times, then completed: identical, nothing left over.
func TestTarStressCancelAndResume(t *testing.T) {
	rng := rand.New(rand.NewSource(16))
	fp := phonetest.NewFS()
	files, mtimes := map[string]string{}, map[string]int64{}
	for i := 0; i < 5000; i++ {
		rel := fmt.Sprintf("d%02d/f%04d.dat", i%37, i)
		n := rng.Intn(4096)
		if i%500 == 0 {
			n = 2<<20 - rng.Intn(1000) // a few files near 2 MB
		}
		files[rel] = strings.Repeat(string(rune('A'+i%26)), n)
		mtimes[rel] = 1_600_000_000 + int64(i)
	}
	addFiles(fp, "/sdcard/S", files, mtimes)
	dst := t.TempDir()
	for round := 0; round < 4; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		stopAt := int64(rng.Intn(3_000_000) + 10_000)
		var once atomic.Bool
		fp.OnStreamRead = func(_ context.Context, n int64) {
			if n > stopAt && once.CompareAndSwap(false, true) {
				cancel()
			}
		}
		p := phonePlan(t, fp, "/sdcard/S", dst, "")
		r, err := Run(ctx, p, Options{Workers: 4, RetryDelay: time.Millisecond})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if r.Failed != 0 {
			t.Fatalf("round %d: %d failures: %+v", round, r.Failed, r.Failures)
		}
		got, _ := listTree(t, filepath.Join(dst, "S"))
		for rel := range got {
			if strings.HasSuffix(rel, PartSuffix) {
				continue // an interrupted file; the next run replaces it
			}
			want, ok := files[filepath.ToSlash(rel)]
			if !ok || int64(len(want)) != got[rel] {
				t.Fatalf("round %d: %s has %d bytes, want %d (a half-written file under a real name?)", round, rel, got[rel], len(want))
			}
		}
	}
	fp.OnStreamRead = nil
	r := mustRun(t, phonePlan(t, fp, "/sdcard/S", dst, ""), fastOpts(4))
	if r.Failed != 0 {
		t.Fatalf("final run: %+v", r)
	}
	checkCopy(t, fp, files, mtimes, filepath.Join(dst, "S"))
}
