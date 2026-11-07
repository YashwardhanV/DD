package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
	"github.com/yashwardhanv/dd/internal/winname"
)

func init() { ui.SetColorMode(false, false) }

// jobsPlan makes a plan of files with the given destination paths and sizes,
// written to a fresh folder on a pretend file system.
func jobsPlan(t *testing.T, fsName string, files map[string]int64) *copier.Plan {
	t.Helper()
	return jobsPlanAt(t, t.TempDir(), fsName, files)
}

// jobsPlanAt is jobsPlan into an existing destination folder.
func jobsPlanAt(t *testing.T, dst, fsName string, files map[string]int64) *copier.Plan {
	t.Helper()
	var jobs []copier.Job
	var names []string
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		jobs = append(jobs, copier.Job{Src: "/nowhere/" + rel, Rel: filepath.FromSlash(rel), Size: files[rel], ModTime: 1_700_000_000})
	}
	p := copier.PlanJobs(dst, jobs, "", 2, fsinfo.ForFileSystem("NTFS"))
	p.DstFS = fsinfo.ForFileSystem(fsName)
	return p
}

func free(n uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return n, nil }
}

// TestIssueMessages is the golden test: the exact text of each issue code.
func TestIssueMessages(t *testing.T) {
	defer func(v bool) { checkLongPaths = v }(checkLongPaths)
	checkLongPaths = true // the long-path case, on any OS
	cases := []struct {
		name     string
		fs       string
		files    map[string]int64
		opt      Options
		code     string
		severity Severity
		message  string
		fixed    string
	}{
		{"no space", "NTFS", map[string]int64{"big.bin": 10 << 20}, Options{FreeBytes: free(1 << 20)},
			"no-space", Block, "The copy needs 10.1 MB on the destination but only 1.0 MB is free.", ""},
		{"fat32 4 GiB", "FAT32", map[string]int64{"film.mkv": 5 << 30}, Options{},
			"fat32-4gib", Block, "The destination is FAT32, which can't store a file of 4 GB or more (1 file).", ""},
		{"case collision", "NTFS", map[string]int64{"Photos/a.jpg": 1, "Photos/A.jpg": 2}, Options{},
			"case-collision", Warn, "1 name differs only in letter case from another, which is the same name on this destination.", `renamed with " (case 2)"`},
		{"case collision fail", "NTFS", map[string]int64{"a.jpg": 1, "A.jpg": 2}, Options{Names: copier.NameOptions{OnCaseCollision: copier.OnCaseFail}},
			"case-collision", Block, "1 name differs only in letter case from another, which is the same name on this destination.", ""},
		{"bad name", "exFAT", map[string]int64{"notes:1.txt": 3}, Options{},
			"bad-name", Warn, "1 name has characters or a form Windows can't store (such as : ? * or a trailing dot).", "replaced with look-alike characters"},
		{"too long", "NTFS", map[string]int64{strings.Repeat("x", 300) + ".txt": 3}, Options{},
			"name-too-long", Warn, "1 name is longer than the 255 characters Windows allows.", "shortened, keeping the extension and a ~hash mark"},
		{"too long fail", "NTFS", map[string]int64{strings.Repeat("x", 300) + ".txt": 3}, Options{Names: copier.NameOptions{OnBadName: copier.OnBadFail}},
			"name-too-long", Block, "1 name is longer than the 255 characters Windows allows.", ""},
		{"long path", "NTFS", map[string]int64{strings.Repeat("folder/", 40) + "f.txt": 3}, Options{},
			"long-path", Warn, "1 file will have a full path longer than 259 characters; DriveDex writes them fine, but Explorer and many programs can't open them.", ""},
		{"slack", "exFAT", map[string]int64{"a": 1000, "b": 1000, "c": 1000}, Options{},
			"cluster-slack", Info, "2.9 KB of files will use 97.0 KB on this exFAT drive (32.0 KB clusters).", ""},
		{"slow port", "NTFS", map[string]int64{"a": 1}, Options{SlowPorts: []SlowPort{{Drive: "E:", Advice: "can run at USB 3 but is running at USB 2.0", Tip: "try another port"}}},
			"slow-port", Info, "E: can run at USB 3 but is running at USB 2.0", ""},
		{"offline", "NTFS", map[string]int64{"a": 1}, Options{SourceOffline: "SANDISK 32GB"},
			"source-offline", Block, "SANDISK 32GB isn't connected, so its files can't be read now.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := Run(jobsPlan(t, c.fs, c.files), c.opt)
			is, ok := rep.Get(c.code)
			if !ok {
				t.Fatalf("no %s issue in %+v", c.code, rep.Issues)
			}
			if is.Severity != c.severity || is.Message != c.message || is.Fixed != c.fixed {
				t.Errorf("got  %v %q (fixed %q)\nwant %v %q (fixed %q)", is.Severity, is.Message, is.Fixed, c.severity, c.message, c.fixed)
			}
			if (c.severity == Block) != rep.Blocked() {
				t.Errorf("Blocked() = %v", rep.Blocked())
			}
		})
	}
}

func TestReportJSONShape(t *testing.T) {
	rep := Run(jobsPlan(t, "FAT32", map[string]int64{"a.jpg": 1, "A.jpg": 2, "film.mkv": 5 << 30}), Options{
		Src: Device{Class: ClassSSD}, Dst: Device{Class: ClassFlash},
	})
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	issues := back["issues"].([]any)
	first := issues[0].(map[string]any)
	if first["code"] != "fat32-4gib" || first["severity"] != "block" {
		t.Errorf("first issue = %v (most serious first)", first)
	}
	for _, key := range []string{"estimate", "renames", "file_bytes", "on_disk_bytes"} {
		if _, ok := back[key]; !ok {
			t.Errorf("JSON lacks %q: %s", key, b)
		}
	}
	est := back["estimate"].(map[string]any)
	if est["seconds"].(float64) <= 0 {
		t.Errorf("estimate seconds = %v", est["seconds"])
	}
}

// TestPhoneFixtureCopiesEveryFileAndReportsRenames is the end-to-end check
// from the design: a phone folder holding a.jpg and A.jpg, notes:1.txt,
// CON.txt and "trailing." copies to a Windows folder with every file present.
func TestPhoneFixtureCopiesEveryFileAndReportsRenames(t *testing.T) {
	fp := phonetest.NewFS()
	files := map[string]string{"a.jpg": "lower", "A.jpg": "UPPER", "notes:1.txt": "notes", "CON.txt": "con", "trailing.": "dot"}
	for name, content := range files {
		fp.Add("/sdcard/Stuff/"+name, content, 1_700_000_000)
	}
	dst := t.TempDir()
	p, err := copier.PlanPhone(context.Background(), fp.ADB(), "R58M1234ABC", "/sdcard/Stuff", dst, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	p.DstFS = fsinfo.ForFileSystem("NTFS") // as on the owner's PC (the tests may run on Linux)
	rep := Run(p, Options{})
	if rep.Blocked() {
		t.Fatalf("blocked: %+v", rep.Issues)
	}
	if !rep.Has("case-collision") || !rep.Has("bad-name") {
		t.Errorf("issues = %+v", rep.Issues)
	}
	if len(rep.Renames) != 4 {
		t.Errorf("renames = %+v, want 4 (A.jpg, notes:1.txt, CON.txt, trailing.)", rep.Renames)
	}
	res, err := copier.Run(context.Background(), p, copier.Options{Workers: 2, RetryDelay: time.Millisecond})
	if err != nil || res.Failed != 0 || res.Written != 5 {
		t.Fatalf("copy: %+v %v", res, err)
	}
	// "A.jpg" sorts before "a.jpg", so it keeps its name.
	want := map[string]string{
		"A.jpg":          "UPPER",
		"a (case 2).jpg": "lower",
		"notes：1.txt":    "notes",
		"CON_.txt":       "con",
		"trailing_":      "dot",
	}
	for name, content := range want {
		b, err := os.ReadFile(filepath.Join(dst, "Stuff", name))
		if err != nil || string(b) != content {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dst, "Stuff"))
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	if strings.Join(got, "|") != "A.jpg|CON_.txt|a (case 2).jpg|notes\uFF1A1.txt|trailing_" {
		t.Errorf("destination holds %q", got)
	}
}

func TestCaseCollisionWithAnExistingDestinationFile(t *testing.T) {
	// The destination already has "photo.jpg" (other letter case).
	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "photo.jpg"), []byte("old"), 0o644)
	p := jobsPlanAt(t, dst, "NTFS", map[string]int64{"Photo.jpg": 5})
	rep := Run(p, Options{})
	if !rep.Has("case-collision") || len(p.Items) != 1 || p.Items[0].Rel != "Photo (case 2).jpg" {
		t.Errorf("issues %+v, item %+v", rep.Issues, p.Items)
	}
	// The same name in the same case is not a collision: it is the same file.
	dst = t.TempDir()
	os.WriteFile(filepath.Join(dst, "photo.jpg"), []byte("old"), 0o644)
	p = jobsPlanAt(t, dst, "NTFS", map[string]int64{"photo.jpg": 5})
	if rep := Run(p, Options{}); rep.Has("case-collision") {
		t.Errorf("identical names collided: %+v", rep.Issues)
	}
}

func TestCaseCollisionSkipModeDropsTheSecondFile(t *testing.T) {
	p := jobsPlan(t, "NTFS", map[string]int64{"x/a.jpg": 1, "x/A.jpg": 2, "X/b.jpg": 3})
	rep := Run(p, Options{Names: copier.NameOptions{OnCaseCollision: copier.OnCaseSkip}})
	is, _ := rep.Get("case-collision")
	if is.Fixed == "" || len(p.Items) != 1 {
		t.Errorf("items %+v issue %+v", p.Items, is)
	}
}

func TestCaseSensitiveDestinationHasNoCollisions(t *testing.T) {
	p := jobsPlan(t, "NTFS", map[string]int64{"a.jpg": 1, "A.jpg": 2})
	p.DstFS.CaseSensitive = true
	if rep := Run(p, Options{}); rep.Has("case-collision") || len(p.Items) != 2 {
		t.Errorf("case-sensitive destination: %+v", rep.Issues)
	}
	// New folders made case-sensitive by --case-sensitive-dest: no collision either.
	p = jobsPlan(t, "NTFS", map[string]int64{"new/a.jpg": 1, "new/A.jpg": 2})
	if rep := Run(p, Options{Names: copier.NameOptions{CaseSensitiveDest: true}}); rep.Has("case-collision") {
		t.Errorf("--case-sensitive-dest: %+v", rep.Issues)
	}
}

func TestBadFolderNameRenamesEverythingInside(t *testing.T) {
	p := jobsPlan(t, "NTFS", map[string]int64{"Trip: Goa/1.jpg": 1, "Trip: Goa/2.jpg": 2})
	rep := Run(p, Options{})
	if len(rep.Renames) != 1 {
		t.Errorf("renames %+v, want just the folder", rep.Renames)
	}
	for _, it := range p.Items {
		if !strings.HasPrefix(it.Rel, "Trip： Goa") {
			t.Errorf("%s not moved with its folder", it.Rel)
		}
	}
}

// TestSlackIsCountedWith128KClusters: 100 files of 1 KB on an exFAT drive with
// 128 KB clusters take 12.5 MB, not 100 KB.
func TestSlackIsCountedWith128KClusters(t *testing.T) {
	files := map[string]int64{}
	for i := 0; i < 100; i++ {
		files[fmt.Sprintf("f%03d", i)] = 1024
	}
	p := jobsPlan(t, "exFAT", files)
	p.DstFS.ClusterBytes = 128 << 10
	rep := Run(p, Options{FreeBytes: free(10 << 20)})
	if rep.FileBytes != 100*1024 || rep.OnDiskBytes < 100*128<<10 {
		t.Errorf("file bytes %d, on disk %d", rep.FileBytes, rep.OnDiskBytes)
	}
	if !rep.Has("no-space") || !rep.Has("cluster-slack") {
		t.Errorf("12.8 MB on disk into 10 MB free must block: %+v", rep.Issues)
	}
	// On NTFS, tiny files live in the MFT and need no cluster.
	small := jobsPlan(t, "NTFS", map[string]int64{"a": 100, "b": 200})
	if _, onDisk := small.SpaceNeeded(); onDisk != 0 {
		t.Errorf("resident NTFS files on disk = %d, want 0", onDisk)
	}
}

func TestDryRunDoesNotProbeTheDestination(t *testing.T) {
	p := jobsPlan(t, "NTFS", map[string]int64{"a": 1})
	Run(p, Options{DryRun: true})
	if entries, _ := os.ReadDir(p.DstRoot); len(entries) != 0 {
		t.Errorf("a dry run wrote %v (INV-6)", entries)
	}
	p.DstFS.ReadOnly = true
	if rep := Run(p, Options{DryRun: true}); !rep.Has("read-only") {
		t.Error("a read-only volume must block even in a dry run")
	}
}

func TestPrintLimitsPathsUnlessVerbose(t *testing.T) {
	files := map[string]int64{}
	for i := 0; i < 12; i++ {
		files[fmt.Sprintf("bad%02d?.txt", i)] = 1
	}
	rep := Run(jobsPlan(t, "NTFS", files), Options{})
	var short, long bytes.Buffer
	rep.Print(&short, false)
	rep.Print(&long, true)
	if !strings.Contains(short.String(), "… and 7 more") || strings.Count(long.String(), "→") != 12 {
		t.Errorf("short:\n%s\nlong:\n%s", short.String(), long.String())
	}
}

// ---- estimate -------------------------------------------------------------------

func TestEstimateFixedInputs(t *testing.T) {
	ssd, flash := Device{Class: ClassSSD}, Device{Class: ClassFlash}
	// 1.5 GB to a pen drive: 1.5e9/300e6 + 1.5e9/15e6 = 5 + 100 s, plus 100 files x 8.3 ms.
	e := EstimateCopy(ssd, flash, 1_500_000_000, 100, copier.VerifyOff, nil)
	if want := 105 + 100*0.0083; abs(e.Seconds-want) > 0.01 {
		t.Errorf("seconds = %.3f, want %.3f", e.Seconds, want)
	}
	if e.Learned || e.Low != e.Seconds*0.75 || e.High != e.Seconds*1.5 {
		t.Errorf("range %+v", e)
	}
	// A pen drive on USB 2.0 that could do USB 3: the link (480 Mbit/s x 0.7 = 42 MB/s)
	// doesn't limit a 15 MB/s writer, so no faster-port line.
	slowFlash := Device{Class: ClassFlash, HasUSB: true, USB: volume.USBLink{Speed: volume.SpeedHigh, Capable: volume.SpeedSuper}}
	if e := EstimateCopy(ssd, slowFlash, 1_500_000_000, 1, copier.VerifyOff, nil); e.FastSeconds != 0 {
		t.Errorf("a slow writer gains nothing from USB 3: %+v", e)
	}
	// A USB SSD on USB 2.0 is limited by the link: USB 3 would be much faster.
	slowSSD := Device{Class: ClassSSD, HasUSB: true, USB: volume.USBLink{Speed: volume.SpeedHigh, Capable: volume.SpeedSuper}}
	e = EstimateCopy(ssd, slowSSD, 10_000_000_000, 1, copier.VerifyOff, nil)
	if e.FastSeconds == 0 || e.FastSeconds > e.Seconds/3 {
		t.Errorf("USB 2.0 SSD: %+v", e)
	}
	if txt := e.Text("E:"); !strings.Contains(txt, "on a USB 3 port it would take about") {
		t.Errorf("text %q", txt)
	}
	// Verification adds one read of everything from the destination.
	v := EstimateCopy(ssd, Device{Class: ClassHDD}, 1<<30, 1, copier.VerifyMedia, nil)
	if abs(v.VerifySeconds-10.737) > 0.01 || v.VerifyText("E:") != "verify on: adds a read of 1.0 GB from E: (about 11 s)" {
		t.Errorf("verify %+v %q", v, v.VerifyText("E:"))
	}
}

type fakeRates map[string]float64

func (f fakeRates) Learned(id string, write bool) (float64, time.Duration, bool) {
	k := id + map[bool]string{true: ":w", false: ":r"}[write]
	v, ok := f[k]
	return v, 0, ok
}

func TestEstimateUsesLearnedRatesAndDropsTheRange(t *testing.T) {
	e := EstimateCopy(Device{Class: ClassSSD}, Device{ID: "vol-1", Class: ClassFlash}, 600_000_000, 0, copier.VerifyOff, fakeRates{"vol-1:w": 60e6})
	if !e.Learned || abs(e.Seconds-(2+10)) > 0.01 || e.Low != e.High {
		t.Errorf("learned estimate %+v", e)
	}
	if txt := e.Text(""); txt != "About 12 s." {
		t.Errorf("text %q", txt)
	}
}

func TestRoughDurations(t *testing.T) {
	for sec, want := range map[float64]string{0.4: "a second", 42: "42 s", 790: "13 min", 4800: "1 h 20 min", 7199: "2 h"} {
		if got := roughDuration(sec); got != want {
			t.Errorf("roughDuration(%v) = %q, want %q", sec, got, want)
		}
	}
	if got := roughRange(480, 1200); got != "8 to 20 min" {
		t.Errorf("roughRange = %q", got)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// FuzzNamesAreValidAndCollisionFree: whatever names arrive (phones allow
// almost anything), FixNames leaves a set that is valid on Windows, has no two
// names equal ignoring case, and keeps names that were already fine.
func FuzzNamesAreValidAndCollisionFree(f *testing.F) {
	f.Add("a.jpg", "A.jpg", "notes:1.txt", "CON")
	f.Add("x.", "x_", "X.", "x")
	f.Add("A (case 2).jpg", "a.jpg", "A.jpg", "a (CASE 2).jpg")
	f.Fuzz(func(t *testing.T, a, b, c, d string) {
		var jobs []copier.Job
		seen := map[string]bool{}
		for _, n := range []string{a, b, c, d} {
			if n == "" || strings.ContainsAny(n, `/\`) || seen[n] || n == "." || n == ".." {
				continue
			}
			seen[n] = true
			jobs = append(jobs, copier.Job{Src: "/x/" + n, Rel: n, Size: 1})
		}
		if len(jobs) == 0 {
			return
		}
		p := copier.PlanJobs(filepath.Join(t.TempDir(), "dst"), jobs, "", 1, fsinfo.ForFileSystem("NTFS"))
		p.DstFS = fsinfo.ForFileSystem("NTFS")
		p.FixNames(copier.NameOptions{})
		folded := map[string]string{}
		for _, it := range p.Items {
			name := it.Rel
			if !winname.Valid(name) {
				t.Fatalf("invalid name %q", name)
			}
			if prev, dup := folded[winname.Fold(name)]; dup {
				t.Fatalf("%q and %q are the same name on Windows", prev, name)
			}
			folded[winname.Fold(name)] = name
			if !it.Renamed && it.Rel != filepath.Base(it.Src) {
				t.Fatalf("name changed without being marked renamed: %q", it.Rel)
			}
		}
		if len(p.Items) != len(jobs) {
			t.Fatalf("%d of %d files kept", len(p.Items), len(jobs))
		}
		// A name that was valid and unique keeps its name.
		for _, j := range jobs {
			if !winname.Valid(j.Rel) {
				continue
			}
			unique := true
			for _, o := range jobs {
				if o.Rel != j.Rel && winname.Fold(o.Rel) == winname.Fold(j.Rel) {
					unique = false
				}
			}
			if !unique {
				continue
			}
			for _, it := range p.Items {
				if it.Src == j.Src && it.Rel != j.Rel {
					t.Fatalf("valid unique name %q was changed to %q", j.Rel, it.Rel)
				}
			}
		}
	})
}
