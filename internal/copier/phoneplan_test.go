package copier

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// phoneNow is the phone's (and the PC's) clock in these tests.
var phoneNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// indexedPhone makes a fake phone with a camera folder and a saved index of
// it made `age` ago.
func indexedPhone(t *testing.T, age time.Duration) (*phonetest.FS, *index.Index) {
	t.Helper()
	fp := phonetest.NewFS()
	fp.Now = func() int64 { return phoneNow.Unix() }
	old := phoneNow.Add(-30 * 24 * time.Hour).Unix()
	for _, n := range []string{"a", "b", "c"} {
		fp.Add("/sdcard/DCIM/Camera/"+n+".jpg", "photo "+n, old)
	}
	fp.Add("/sdcard/Music/x.mp3", "music", old)
	b := index.NewBuilder(index.Header{SourceKind: index.KindPhone, SourceID: "adb-R58M1234ABC", RootPath: "/sdcard", UpdatedAt: phoneNow.Add(-age)})
	if err := phone.NewScanner(fp.ADB(), "R58M1234ABC", "/sdcard/").Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	ix := b.Index()
	ix.Header.UpdatedAt = phoneNow.Add(-age)
	return fp, ix
}

func smartPlan(t *testing.T, fp *phonetest.FS, ix *index.Index, mode, dst string) (PhonePlanResult, error) {
	t.Helper()
	return PlanPhoneSmart(context.Background(), PhonePlanRequest{
		ADB: fp.ADB(), Serial: "R58M1234ABC", Remote: "/sdcard/DCIM/Camera", DstRoot: dst,
		Policy: OnConflictNewer, Workers: 4, Index: ix, Mode: mode, Now: phoneNow,
	})
}

func TestPhonePlanFromIndexPlusDelta(t *testing.T) {
	fp, ix := indexedPhone(t, 12*time.Minute)
	recent := phoneNow.Add(-3 * time.Minute).Unix()
	// Since the index: b edited, d new, c deleted, and an old photo moved in (old mtime, new ctime).
	fp.Add("/sdcard/DCIM/Camera/b.jpg", "photo b, edited", recent)
	fp.Add("/sdcard/DCIM/Camera/d.jpg", "photo d", recent)
	fp.Remove("/sdcard/DCIM/Camera/c.jpg")
	fp.Add("/sdcard/DCIM/Camera/moved.jpg", "moved here", phoneNow.Add(-90*24*time.Hour).Unix())
	fp.SetCTime("/sdcard/DCIM/Camera/moved.jpg", recent)

	dst := t.TempDir()
	res, err := smartPlan(t, fp, ix, "auto", dst)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Source != PlanPhoneIndex || !res.Plan.MayBeStale {
		t.Fatalf("source %q", res.Plan.Source)
	}
	if !strings.Contains(res.Why, "Planned from the phone index (made 12 minutes ago) + 3 changed files.") {
		t.Errorf("why: %q", res.Why)
	}
	// The delta asked about the last 12 + 2 minutes only, not a full find.
	var sawDelta, sawFull bool
	for _, c := range fp.ShellCommands() {
		sawDelta = sawDelta || strings.Contains(c, "-cmin -14 ")
		sawFull = sawFull || (strings.HasPrefix(c, "find /sdcard/DCIM") && !strings.Contains(c, "-cmin"))
	}
	if !sawDelta || sawFull {
		t.Errorf("delta %v full %v: %q", sawDelta, sawFull, fp.ShellCommands())
	}
	if len(res.Changed) != 3 || res.CoversIndex {
		t.Errorf("changed %+v covers %v", res.Changed, res.CoversIndex)
	}

	r := mustRun(t, res.Plan, fastOpts(4))
	if r.Vanished != 1 || r.Failed != 0 || r.Written != 4 {
		t.Errorf("result %+v", r)
	}
	got, _ := listTree(t, filepath.Join(dst, "Camera"))
	want := map[string]int64{"a.jpg": 7, "b.jpg": 15, "d.jpg": 7, "moved.jpg": 10}
	if len(got) != len(want) {
		t.Fatalf("destination %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d bytes, want %d", k, got[k], v)
		}
	}
}

func TestPhonePlanFallsBackToFind(t *testing.T) {
	cases := []struct {
		name  string
		setup func(fp *phonetest.FS, ix **index.Index)
		mode  string
		why   string
	}{
		{"no index", func(fp *phonetest.FS, ix **index.Index) { *ix = nil }, "auto", "no saved index"},
		{"no -cmin", func(fp *phonetest.FS, ix **index.Index) { fp.NoCmin = true }, "auto", "-cmin"},
		{"too old", func(fp *phonetest.FS, ix **index.Index) { (*ix).Header.UpdatedAt = phoneNow.Add(-8 * 24 * time.Hour) }, "auto", "made 8 days ago"},
		{"--fresh", func(fp *phonetest.FS, ix **index.Index) {}, "find", "fresh listing"},
		{"names only", func(fp *phonetest.FS, ix **index.Index) {
			for i := range (*ix).Flags {
				if (*ix).Flags[i]&index.FlagDir == 0 {
					(*ix).Flags[i] |= index.FlagNoMeta
				}
			}
		}, "auto", "no sizes and dates"},
		{"outside the index", func(fp *phonetest.FS, ix **index.Index) { (*ix).Header.RootPath = "/storage/1234-ABCD" }, "auto", "outside the indexed part"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp, ix := indexedPhone(t, 10*time.Minute)
			c.setup(fp, &ix)
			res, err := smartPlan(t, fp, ix, c.mode, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if res.Plan.Source != PlanPhoneFind || res.Plan.MayBeStale || !strings.Contains(res.Why, c.why) {
				t.Errorf("source %q why %q", res.Plan.Source, res.Why)
			}
			if len(res.Plan.Items) != 3 {
				t.Errorf("%d items", len(res.Plan.Items))
			}
		})
	}
}

func TestPhonePlanForcedIndexExplainsWhyNot(t *testing.T) {
	fp, ix := indexedPhone(t, time.Minute)
	fp.NoCmin = true
	if _, err := smartPlan(t, fp, ix, "index", t.TempDir()); !errors.Is(err, ErrIndexNotUsable) || !strings.Contains(err.Error(), "-cmin") {
		t.Errorf("err %v", err)
	}
}

func TestPhoneCminProbeIsCachedPerPhone(t *testing.T) {
	fp, ix := indexedPhone(t, time.Minute)
	adb := fp.ADB()
	for i := 0; i < 3; i++ {
		if _, err := PlanPhoneSmart(context.Background(), PhonePlanRequest{ADB: adb, Serial: "R58M1234ABC", Remote: "/sdcard/DCIM/Camera",
			DstRoot: t.TempDir(), Index: ix, Mode: "auto", Now: phoneNow}); err != nil {
			t.Fatal(err)
		}
	}
	probes := 0
	for _, c := range fp.ShellCommands() {
		if strings.Contains(c, "-maxdepth 0 -cmin") {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("probed %d times, want once", probes)
	}
}

func TestPhoneIndexUpsertAfterTheCopy(t *testing.T) {
	fp, ix := indexedPhone(t, 5*time.Minute)
	fp.Add("/sdcard/DCIM/Camera/new.jpg", "brand new", phoneNow.Add(-time.Minute).Unix())
	res, err := smartPlan(t, fp, ix, "auto", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n := ix.UpsertPaths(res.Changed); n != 1 {
		t.Fatalf("upserted %d", n)
	}
	id, ok := ix.FindFolder("DCIM/Camera")
	if !ok {
		t.Fatal("folder gone")
	}
	found := false
	ix.WalkFolder(ix.BuildChildren(), id, func(slot int, rel string) bool {
		found = found || rel == "new.jpg"
		return true
	})
	if !found {
		t.Error("the new file is not in the index after the upsert")
	}
	_ = os.Remove
}

func TestPhoneRel(t *testing.T) {
	cases := []struct {
		root, remote, want string
		ok                 bool
	}{
		{"/sdcard", "/sdcard/DCIM/Camera", "DCIM/Camera", true},
		{"/sdcard/", "/sdcard", "", true},
		{"/sdcard", "/sdcardX/DCIM", "", false},
		{"/sdcard", "/storage/emulated/0/DCIM", "", false},
		{"/", "/sdcard/DCIM", "sdcard/DCIM", true},
	}
	for _, c := range cases {
		got, ok := phoneRel(c.root, c.remote)
		if got != c.want || ok != c.ok {
			t.Errorf("phoneRel(%q, %q) = %q, %v", c.root, c.remote, got, ok)
		}
	}
}
