package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func healthy() Facts {
	return Facts{
		Version: "1.0.0", Now: now, StaleAfter: 24 * time.Hour,
		IndexDir: `C:\Data`, IndexCount: 2, ConfigPath: `C:\Cfg\config.toml`,
		AgentKnown: true, AgentRunning: true, StartupKnown: true, StartupEnabled: true,
		MenuKnown: true, MenuInstalled: 4, MenuTotal: 4, Elevated: true,
		ADB: ADBFacts{Found: true, Path: `C:\adb.exe`, Version: "Android Debug Bridge version 1.0.41",
			Phones: []PhoneFacts{{Serial: "R58", Name: "Pixel 7", State: "device"}}},
		Drives: []DriveFacts{{
			Letter: "E:", Label: "SANDISK", FileSystem: "NTFS", Bus: "USB", Media: MediaFlash,
			Index: &IndexFacts{UpdatedAt: now.Add(-time.Hour), Entries: 123456, Scanner: "mft"}, Journal: JournalActive,
		}},
	}
}

func lines(r Report) []Line {
	var out []Line
	for _, s := range r.Sections {
		out = append(out, s.Lines...)
	}
	return out
}

func find(t *testing.T, r Report, substr string) Line {
	t.Helper()
	for _, l := range lines(r) {
		if strings.Contains(l.Text, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, render(r))
	return Line{}
}

func render(r Report) string {
	var b bytes.Buffer
	r.Print(&b)
	return b.String()
}

func TestHealthySystemHasNoWarnings(t *testing.T) {
	r := Build(healthy())
	ok, warn, fail := r.Counts()
	if warn != 0 || fail != 0 || ok == 0 {
		t.Errorf("ok=%d warn=%d fail=%d, want all ok:\n%s", ok, warn, fail, render(r))
	}
	for _, l := range lines(r) {
		if l.Level == OK && l.Tip != "" {
			t.Errorf("an OK line shouldn't carry a fix tip: %+v", l)
		}
	}
}

func TestEveryProblemHasAFixTip(t *testing.T) {
	f := healthy()
	f.IndexDirErr = errors.New("access denied")
	f.ConfigErr = errors.New("line 3: bad value")
	f.Elevated = false
	f.ADB.Phones = []PhoneFacts{
		{Serial: "A", Name: "Pixel", State: "unauthorized"},
		{Serial: "B", State: "offline"},
		{Serial: "C", State: "recovery"},
	}
	f.Drives[0].FileSystem = "FAT32"
	f.Drives[0].Index = nil
	f.MenuInstalled = 2
	r := Build(f)
	for _, l := range lines(r) {
		if (l.Level == Warn || l.Level == Fail) && l.Tip == "" {
			t.Errorf("%v line without a tip: %q", l.Level, l.Text)
		}
	}
	if _, warn, fail := r.Counts(); fail < 3 || warn < 3 {
		t.Errorf("expected several findings, got warn=%d fail=%d:\n%s", warn, fail, render(r))
	}
}

func TestPrivilegesExplainTurboMode(t *testing.T) {
	f := healthy()
	if l := find(t, Build(f), "Administrator"); l.Level != OK || !strings.Contains(l.Text, "turbo") {
		t.Errorf("elevated: %+v", l)
	}
	f.Elevated = false
	l := find(t, Build(f), "not running as Administrator")
	if l.Level != Info || !strings.Contains(l.Text, "turbo mode") || !strings.Contains(l.Tip, "Run as administrator") {
		t.Errorf("not elevated: %+v", l)
	}
}

func TestPhoneStates(t *testing.T) {
	cases := []struct {
		state string
		level Level
		tip   string
	}{
		{"device", OK, ""},
		{"unauthorized", Fail, "Allow"},
		{"offline", Warn, "adb kill-server"},
		{"no permissions", Warn, "unplug"},
	}
	for _, c := range cases {
		f := healthy()
		f.ADB.Phones = []PhoneFacts{{Serial: "X", Name: "Pixel 7", State: c.state}}
		l := find(t, Build(f), "Pixel 7")
		if l.Level != c.level || !strings.Contains(l.Tip, c.tip) {
			t.Errorf("state %q: got %+v, want level %v tip containing %q", c.state, l, c.level, c.tip)
		}
	}
}

func TestPhoneSectionWithoutAdbOrPhones(t *testing.T) {
	f := healthy()
	f.ADB = ADBFacts{}
	if l := find(t, Build(f), "adb (Android Debug Bridge) was not found"); l.Level != Info {
		t.Errorf("missing adb is information, not a failure: %+v", l)
	}
	f.ADB = ADBFacts{Found: true, Path: "adb"}
	if l := find(t, Build(f), "no phone connected"); l.Level != Info {
		t.Errorf("no phone: %+v", l)
	}
	f.ADB = ADBFacts{Found: true, Path: "adb", Err: errors.New("daemon not running")}
	if l := find(t, Build(f), "couldn't list phones"); l.Level != Fail {
		t.Errorf("adb error: %+v", l)
	}
}

func TestDriveChecks(t *testing.T) {
	t.Run("not indexed", func(t *testing.T) {
		f := healthy()
		f.Drives[0].Index = nil
		if l := find(t, Build(f), "not indexed yet"); l.Level != Warn || !strings.Contains(l.Tip, "drivedex index E:") {
			t.Errorf("%+v", l)
		}
	})
	t.Run("stale index", func(t *testing.T) {
		f := healthy()
		f.Drives[0].Index.UpdatedAt = now.Add(-72 * time.Hour)
		if l := find(t, Build(f), "indexed"); l.Level != Warn || !strings.Contains(l.Tip, "drivedex index E:") {
			t.Errorf("%+v", l)
		}
	})
	t.Run("fresh index shows age and size", func(t *testing.T) {
		l := find(t, Build(healthy()), "indexed")
		if l.Level != OK || !strings.Contains(l.Text, "123,456") || !strings.Contains(l.Text, "mft") {
			t.Errorf("%+v", l)
		}
	})
	t.Run("FAT32 warns about 4 GiB", func(t *testing.T) {
		f := healthy()
		f.Drives[0].FileSystem = "FAT32"
		if l := find(t, Build(f), "FAT32"); l.Level != Warn || !strings.Contains(l.Text, "4 GiB") {
			t.Errorf("%+v", l)
		}
	})
	t.Run("hdd, flash and unknown media", func(t *testing.T) {
		for m, want := range map[Media]string{MediaHDD: "spinning", MediaFlash: "flash", MediaUnknown: "couldn't tell"} {
			f := healthy()
			f.Drives[0].Media = m
			find(t, Build(f), want)
		}
	})
	t.Run("journal states", func(t *testing.T) {
		f := healthy()
		f.Drives[0].Journal = JournalInactive
		if l := find(t, Build(f), "journal is off"); l.Level != Warn || !strings.Contains(l.Tip, "journal enable E:") {
			t.Errorf("%+v", l)
		}
		f.Drives[0].Journal = JournalUnchecked
		f.Elevated = false
		find(t, Build(f), "change journal not checked")
	})
	t.Run("non-NTFS has no journal line", func(t *testing.T) {
		f := healthy()
		f.Drives[0].FileSystem = "exFAT"
		for _, l := range lines(Build(f)) {
			if strings.Contains(l.Text, "journal") {
				t.Errorf("exFAT drives have no change journal, but got %q", l.Text)
			}
		}
	})
	t.Run("one section per drive", func(t *testing.T) {
		f := healthy()
		f.Drives = append(f.Drives, DriveFacts{Letter: "F:", FileSystem: "exFAT"})
		titles := ""
		for _, s := range Build(f).Sections {
			titles += s.Title + "|"
		}
		if !strings.Contains(titles, "Drive E:  SANDISK|Drive F:|") {
			t.Errorf("titles = %q", titles)
		}
	})
}

func TestDrivesErrorAndNoDrives(t *testing.T) {
	f := healthy()
	f.Drives, f.DrivesErr = nil, errors.New("unsupported")
	if l := find(t, Build(f), "couldn't list drives"); l.Level != Warn {
		t.Errorf("%+v", l)
	}
	f.DrivesErr = nil
	find(t, Build(f), "no ready drives")
}

func TestDriveDexSectionFindings(t *testing.T) {
	f := healthy()
	f.AgentRunning, f.StartupEnabled, f.MenuInstalled = false, false, 0
	r := Build(f)
	if l := find(t, r, "agent is not running"); l.Level != Info || !strings.Contains(l.Tip, "agent start") {
		t.Errorf("%+v", l)
	}
	if l := find(t, r, "does not start with Windows"); !strings.Contains(l.Tip, "startup enable") {
		t.Errorf("%+v", l)
	}
	if l := find(t, r, "right-click menu is not installed"); !strings.Contains(l.Tip, "shell install") {
		t.Errorf("%+v", l)
	}
	f.AgentKnown, f.StartupKnown, f.MenuKnown = false, false, false
	for _, l := range lines(Build(f)) {
		if strings.Contains(l.Text, "agent") || strings.Contains(l.Text, "right-click") {
			t.Errorf("unknowable facts must not be reported: %q", l.Text)
		}
	}
}

func TestPrintShowsGlyphsTipsAndSummary(t *testing.T) {
	f := healthy()
	f.Drives[0].Index = nil
	out := render(Build(f))
	for _, s := range []string{"✅", "⚠️", "→ index it with: drivedex index E:", "DriveDex", "Privileges", "Phone", "Drive E:", "ok,", "to look at"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
}

func TestTipsAreInformationOnly(t *testing.T) {
	var b bytes.Buffer
	PrintTips(&b)
	out := b.String()
	for _, s := range []string{"Write caching", "Better performance", "Safely Remove", "Defender", "don't turn it off", "zip"} {
		if !strings.Contains(out, s) {
			t.Errorf("tips missing %q:\n%s", s, out)
		}
	}
}
