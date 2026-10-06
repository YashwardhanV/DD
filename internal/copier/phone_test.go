package copier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

const serial = "R58M1234ABC"

func phonePlan(t *testing.T, fp *phonetest.FS, remote, dst, policy string) *Plan {
	t.Helper()
	p, err := PlanPhone(context.Background(), fp.ADB(), serial, remote, dst, policy, 4)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sampleCamera(fp *phonetest.FS, n int) {
	for i := 0; i < n; i++ {
		fp.Add(fmt.Sprintf("/sdcard/DCIM/Camera/IMG_%04d.jpg", i), strings.Repeat("x", 10+i), 1_700_000_000+int64(i))
	}
}

func TestPhonePathConversion(t *testing.T) {
	cases := map[string]string{
		"/DCIM/Camera":              "/sdcard/DCIM/Camera",
		"DCIM/Camera":               "/sdcard/DCIM/Camera",
		`\DCIM\Camera`:              "/sdcard/DCIM/Camera",
		"/sdcard/DCIM/Camera":       "/sdcard/DCIM/Camera",
		"/sdcard":                   "/sdcard",
		"/":                         "/sdcard",
		"/storage/1234-5678/Music/": "/storage/1234-5678/Music",
		"/DCIM/../Download":         "/sdcard/Download",
	}
	for in, want := range cases {
		if got := PhonePath(in); got != want {
			t.Errorf("PhonePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanPhoneFolder(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 5)
	fp.Add("/sdcard/DCIM/Camera/Sub Folder/clip.mp4", strings.Repeat("v", 500), 1_700_000_100)
	fp.Add("/sdcard/DCIM/Other/not-included.jpg", "zzz", 1)

	p := phonePlan(t, fp, "/sdcard/DCIM/Camera", t.TempDir(), "")
	if p.Phone == nil || p.Phone.Serial != serial || p.Phone.Remote != "/sdcard/DCIM/Camera" {
		t.Fatalf("phone source = %+v", p.Phone)
	}
	if len(p.Items) != 6 {
		t.Fatalf("planned %d files, want 6", len(p.Items))
	}
	if first := p.Items[0]; first.Src != "/sdcard/DCIM/Camera/Sub Folder/clip.mp4" || first.Size != 500 ||
		first.Rel != filepath.Join("Camera", "Sub Folder", "clip.mp4") || first.ModTime != 1_700_000_100 {
		t.Errorf("largest item = %+v", first)
	}
	wantDirs := []string{"Camera", filepath.Join("Camera", "Sub Folder")}
	if strings.Join(p.Dirs, "|") != strings.Join(wantDirs, "|") {
		t.Errorf("dirs = %q, want %q", p.Dirs, wantDirs)
	}
}

func TestPlanPhoneSingleFileAndNotFound(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Add("/sdcard/Download/report.pdf", "PDFDATA", 1_700_000_000)
	p := phonePlan(t, fp, "/sdcard/Download/report.pdf", t.TempDir(), "")
	if len(p.Items) != 1 || p.Items[0].Rel != "report.pdf" || p.Items[0].Size != 7 || p.Items[0].Src != "/sdcard/Download/report.pdf" {
		t.Errorf("items = %+v", p.Items)
	}
	if _, err := PlanPhone(context.Background(), fp.ADB(), serial, "/sdcard/Nope", t.TempDir(), "", 2); !errors.Is(err, ErrRemoteNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPhoneCopyEndToEnd(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 30)
	fp.Add("/sdcard/DCIM/Camera/Sub Folder/clip.mp4", "VIDEO", 1_700_000_500)
	dst := t.TempDir()

	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if r.Written != 31 || r.Failed != 0 || r.Cancelled {
		t.Fatalf("result = %+v", r)
	}
	files, _ := listTree(t, filepath.Join(dst, "Camera"))
	if len(files) != 31 {
		t.Fatalf("copied %d files", len(files))
	}
	got, _ := os.ReadFile(filepath.Join(dst, "Camera", "Sub Folder", "clip.mp4"))
	if string(got) != "VIDEO" {
		t.Errorf("content = %q", got)
	}
	st, _ := os.Stat(filepath.Join(dst, "Camera", "IMG_0007.jpg"))
	if st.ModTime().Unix() != 1_700_000_007 || st.Size() != 17 {
		t.Errorf("IMG_0007: mtime %d size %d", st.ModTime().Unix(), st.Size())
	}
	leftovers(t, dst)

	// Files were pulled grouped by their phone folder: every pull's files share one parent.
	for _, call := range fp.PullCalls() {
		parent := path.Dir(call[0])
		for _, f := range call {
			if path.Dir(f) != parent {
				t.Errorf("a pull mixed folders: %q", call)
			}
		}
	}
}

func TestPhonePullsAreBatchedAtMost100Files(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 250)
	mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", t.TempDir(), ""), fastOpts(1))
	if fp.PullCount() != 3 {
		t.Errorf("250 files should need 3 pulls (100+100+50), got %d", fp.PullCount())
	}
	for _, call := range fp.PullCalls() {
		if len(call) > 100 {
			t.Errorf("a pull had %d files", len(call))
		}
	}
}

func TestPhonePullsRespectTheCommandLineLength(t *testing.T) {
	fp := phonetest.NewFS()
	long := strings.Repeat("n", 230)
	for i := 0; i < 120; i++ {
		fp.Add(fmt.Sprintf("/sdcard/Long/%s%03d.bin", long, i), "d", 1)
	}
	mustRun(t, phonePlan(t, fp, "/sdcard/Long", t.TempDir(), ""), fastOpts(1))
	if fp.PullCount() < 2 {
		t.Errorf("120 files with 260-char paths must be split by length, got %d pull(s)", fp.PullCount())
	}
	for _, call := range fp.PullCalls() {
		total := 0
		for _, f := range call {
			total += len(f) + 1
		}
		if total > maxPullChars+300 {
			t.Errorf("a pull command carried %d characters of paths (limit %d)", total, maxPullChars)
		}
	}
}

func TestPhoneCopyResumes(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 40)
	dst := t.TempDir()
	mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	pulls := fp.PullCount()

	// Nothing changed: the plan skips everything and nothing is pulled.
	p := phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, "")
	if c := p.Counts(); c.Skipped != 40 || c.Copy != 0 {
		t.Fatalf("counts = %+v", c)
	}
	r := mustRun(t, p, fastOpts(1))
	if r.Written != 0 || r.Skipped != 40 || fp.PullCount() != pulls {
		t.Errorf("result = %+v, pulls %d -> %d", r, pulls, fp.PullCount())
	}

	// A file vanished locally (e.g. an interrupted earlier run): only it is pulled.
	os.Remove(filepath.Join(dst, "Camera", "IMG_0013.jpg"))
	r = mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if r.Written != 1 || fp.PullCount() != pulls+1 || len(fp.PullCalls()[fp.PullCount()-1]) != 1 {
		t.Errorf("result = %+v, last pull = %q", r, fp.PullCalls()[fp.PullCount()-1])
	}
}

func TestPhoneBatchWithOneBadFileRetriesThatFileAlone(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 10)
	bad := "/sdcard/DCIM/Camera/IMG_0004.jpg"
	fp.Unreadable[bad] = true
	dst := t.TempDir()

	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if r.Written != 9 || r.Failed != 1 || len(r.Failures) != 1 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.HasSuffix(r.Failures[0].Rel, "IMG_0004.jpg") || !strings.Contains(r.Failures[0].Err, "failed to copy") {
		t.Errorf("failure = %+v", r.Failures[0])
	}
	// 1 batch of 10, then the bad one retried alone 2 times.
	if fp.PullCount() != 3 {
		t.Errorf("pull calls = %d, want 3: %q", fp.PullCount(), fp.PullCalls())
	}
	if _, err := os.Stat(filepath.Join(dst, "Camera", "IMG_0004.jpg")); err == nil {
		t.Error("the unreadable file must not appear")
	}
	leftovers(t, dst)
}

func TestPhoneFileThatWorksOnRetry(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 5)
	fp.FailTimes["/sdcard/DCIM/Camera/IMG_0002.jpg"] = 1 // fails inside the batch, fine on retry
	dst := t.TempDir()
	r := mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if r.Written != 5 || r.Failed != 0 {
		t.Errorf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dst, "Camera", "IMG_0002.jpg")); err != nil {
		t.Error("the retried file must arrive")
	}
}

func TestPhoneInterruptedPullLeavesNothingUnderRealNames(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 20)
	dst := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	fp.OnPull = func(context.Context) { cancel() } // the user presses Ctrl+C mid-pull
	r, err := Run(ctx, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if err != nil || !r.Cancelled || r.Failed != 0 {
		t.Fatalf("result = %+v err = %v", r, err)
	}
	files, _ := listTree(t, filepath.Join(dst, "Camera"))
	if len(files) != 0 {
		t.Errorf("a cancelled pull left real files behind: %v", files)
	}
	leftovers(t, dst)

	// A stale staging folder from a crash is cleaned up by the next run.
	stale := filepath.Join(dst, stagingPrefix+"crashed")
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "half.jpg"), []byte("partial"), 0o644)
	fp.OnPull = nil
	r = mustRun(t, phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, ""), fastOpts(1))
	if r.Written != 20 {
		t.Errorf("rerun result = %+v", r)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale staging folder should be removed")
	}
	leftovers(t, dst)
}

func TestPhoneUsesAtMostTwoParallelPulls(t *testing.T) {
	fp := phonetest.NewFS()
	for d := 0; d < 8; d++ { // 8 folders => 8 batches
		for i := 0; i < 3; i++ {
			fp.Add(fmt.Sprintf("/sdcard/Many/dir%d/f%d.bin", d, i), "data", 1)
		}
	}
	fp.OnPull = func(context.Context) { time.Sleep(20 * time.Millisecond) }
	opt := fastOpts(16) // asking for 16 workers changes nothing for phones
	mustRun(t, phonePlan(t, fp, "/sdcard/Many", t.TempDir(), ""), opt)
	if peak := fp.PeakPulls(); peak != 2 {
		t.Errorf("peak parallel pulls = %d, want exactly 2", peak)
	}
	if fp.PullCount() != 8 {
		t.Errorf("pulls = %d, want 8", fp.PullCount())
	}
}

func TestPhoneConflictPolicyAppliesToLocalFiles(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Add("/sdcard/Docs/a.txt", "PHONE VERSION", 1_700_000_000)
	dst := t.TempDir()
	local := filepath.Join(dst, "Docs", "a.txt")
	os.MkdirAll(filepath.Dir(local), 0o755)
	os.WriteFile(local, []byte("local, edited later"), 0o644) // mtime = now, newer than the phone file

	p := phonePlan(t, fp, "/sdcard/Docs", dst, "newer")
	if p.Items[0].Decision != SkipNewer {
		t.Errorf("decision = %v: a newer local file must be kept", p.Items[0].Decision)
	}
	p = phonePlan(t, fp, "/sdcard/Docs", dst, "overwrite")
	mustRun(t, p, fastOpts(1))
	if got, _ := os.ReadFile(local); string(got) != "PHONE VERSION" {
		t.Errorf("overwrite policy: %q", got)
	}
}

func TestPhoneDestinationFolderBlockingAFileIsAFailure(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Add("/sdcard/D/x.txt", "1", 1)
	fp.Add("/sdcard/D/y.txt", "2", 1)
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, "D", "x.txt"), 0o755)
	r := mustRun(t, phonePlan(t, fp, "/sdcard/D", dst, ""), fastOpts(1))
	if r.Written != 1 || r.Failed != 1 || !strings.Contains(r.Failures[0].Err, "folder") {
		t.Errorf("result = %+v", r)
	}
}
