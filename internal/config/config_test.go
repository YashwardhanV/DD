package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "DriveDex", "config.toml")
	if body != "" {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestDefaultsMatchTheSpec(t *testing.T) {
	d := Defaults()
	want := Config{
		DefaultLimit: 50, ShowHidden: true, Workers: "auto",
		SkipDirs: []string{"System Volume Information", "$RECYCLE.BIN", "$Recycle.Bin"}, SkipDirsExtra: []string{},
		StaleAfter: "24h", Notifications: true, AutoIndex: []string{}, AdbPath: "", PhoneRoot: "/sdcard/", Color: "auto",
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("defaults = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Errorf("the defaults must be valid: %v", err)
	}
}

func TestTheTemplateParsesToExactlyTheDefaults(t *testing.T) {
	p := write(t, Template)
	got, err := Load(p)
	if err != nil {
		t.Fatalf("the shipped template must be valid: %v", err)
	}
	if !reflect.DeepEqual(got, Defaults()) {
		t.Errorf("template gives %+v\nwant %+v", got, Defaults())
	}
}

func TestMissingFileIsCreatedWithTheTemplate(t *testing.T) {
	p := write(t, "")
	cfg, err := Load(p)
	if err != nil || !reflect.DeepEqual(cfg, Defaults()) {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != Template {
		t.Errorf("the config file must be created on first run: %v", err)
	}
	// Second load reads it back.
	if cfg2, err := Load(p); err != nil || !reflect.DeepEqual(cfg2, Defaults()) {
		t.Errorf("reload: %+v %v", cfg2, err)
	}
}

func TestUnwritableLocationIsNotAnError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	cfg, err := Load(filepath.Join(blocker, "sub", "config.toml")) // can't create a folder inside a file
	if err != nil || !reflect.DeepEqual(cfg, Defaults()) {
		t.Errorf("cfg=%+v err=%v", cfg, err)
	}
}

func TestPartialFilesKeepDefaultsForTheRest(t *testing.T) {
	cfg, err := Load(write(t, "default_limit = 200\nshow_hidden = false\nskip_dirs_extra = [\"node_modules\", \".git\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultLimit != 200 || cfg.ShowHidden || !reflect.DeepEqual(cfg.SkipDirsExtra, []string{"node_modules", ".git"}) {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.StaleAfter != "24h" || !cfg.Notifications || cfg.PhoneRoot != "/sdcard/" || len(cfg.SkipDirs) != 3 {
		t.Errorf("defaults lost: %+v", cfg)
	}
	if got := cfg.AllSkipDirs(); len(got) != 5 || got[3] != "node_modules" {
		t.Errorf("AllSkipDirs = %q", got)
	}
}

func TestWorkersAcceptsAStringOrANumber(t *testing.T) {
	for body, want := range map[string]Workers{`workers = "auto"`: "auto", `workers = 8`: "8", `workers = "12"`: "12", `workers = " AUTO "`: "AUTO"} {
		cfg, err := Load(write(t, body))
		if err != nil || cfg.Workers != want {
			t.Errorf("%s: %q %v", body, cfg.Workers, err)
		}
	}
	if _, err := Load(write(t, "workers = 1.5\n")); err == nil {
		t.Error("a fraction isn't a worker count")
	}
	if _, err := Load(write(t, "workers = true\n")); err == nil {
		t.Error("a boolean isn't a worker count")
	}
}

func TestParseWorkers(t *testing.T) {
	cases := map[string]struct {
		n    int
		auto bool
		ok   bool
	}{
		"auto": {0, true, true}, "": {0, true, true}, "AUTO": {0, true, true}, "8": {8, false, true}, " 3 ": {3, false, true},
		"0": {0, false, false}, "-1": {0, false, false}, "257": {0, false, false}, "many": {0, false, false}, "4x": {0, false, false},
	}
	for in, w := range cases {
		n, auto, err := ParseWorkers(in)
		if (err == nil) != w.ok || (w.ok && (n != w.n || auto != w.auto)) {
			t.Errorf("ParseWorkers(%q) = %d,%v,%v want %+v", in, n, auto, err, w)
		}
	}
}

func TestValidationMessagesTellYouWhatToWrite(t *testing.T) {
	cases := map[string]string{
		"default_limit = -1":        "default_limit must be 0 or more",
		`color = "rainbow"`:         `color must be "auto", "always" or "never"`,
		`stale_after = "tomorrow"`:  `stale_after must be a time like "24h"`,
		`auto_index = ["D:", "12"]`: `auto_index entries must be drive letters like "D:"`,
		`phone_root = "sdcard"`:     "phone_root must start with /",
		`workers = "lots"`:          `workers must be "auto" or a number`,
	}
	for body, want := range cases {
		_, err := Load(write(t, body+"\n"))
		var ce *Error
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want mention of %q", body, err, want)
		}
	}
}

func TestSyntaxErrorsNameTheLine(t *testing.T) {
	_, err := Load(write(t, "default_limit = 10\nshow_hidden = maybe\nworkers = \"auto\"\n"))
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "line 2") || !strings.Contains(ce.Path, "config.toml") {
		t.Errorf("err = %v", err)
	}
	// Wrong type for a known key.
	_, err = Load(write(t, "default_limit = \"fifty\"\n"))
	if err == nil {
		t.Error("a string where a number belongs must be rejected")
	}
	// On error, callers get safe defaults, never a half-filled config.
	cfg, _ := Load(write(t, "default_limit = 7\nthis is not toml\n"))
	if !reflect.DeepEqual(cfg, Defaults()) {
		t.Errorf("a broken file must give the defaults: %+v", cfg)
	}
}

func TestStaleDuration(t *testing.T) {
	cases := map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "7d": 7 * 24 * time.Hour, "0.5d": 12 * time.Hour, "": 24 * time.Hour, "0s": 0}
	for in, want := range cases {
		got, err := Config{StaleAfter: in}.StaleDuration()
		if err != nil || got != want {
			t.Errorf("StaleDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1h", "soon", "5x"} {
		if _, err := (Config{StaleAfter: bad}).StaleDuration(); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestNormalizeTidiesValues(t *testing.T) {
	cfg, err := Load(write(t, "color = \"ALWAYS\"\nauto_index = [\"d\", \"e:\", \"F:\\\\\"]\nphone_root = \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Color != "always" || !reflect.DeepEqual(cfg.AutoIndex, []string{"D:", "E:", "F:"}) || cfg.PhoneRoot != "/sdcard/" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestPathHonoursTheEnvironmentVariable(t *testing.T) {
	t.Setenv(EnvPath, "/custom/config.toml")
	if p, err := Path(); err != nil || p != "/custom/config.toml" {
		t.Errorf("Path = %q %v", p, err)
	}
	t.Setenv(EnvPath, "")
	if p, err := Path(); err != nil || !strings.HasSuffix(filepath.ToSlash(p), "DriveDex/config.toml") {
		t.Errorf("default Path = %q %v", p, err)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	text, err := Defaults().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(write(t, text))
	if err != nil || !reflect.DeepEqual(cfg, Defaults()) {
		t.Errorf("round trip: %+v %v\n%s", cfg, err, text)
	}
}

// A huge stale_after in days used to overflow into a NEGATIVE duration, which
// made every index count as stale - the opposite of what was asked.
func TestHugeStaleAfterMeansNeverStale(t *testing.T) {
	for _, s := range []string{"300000d", "1e10d", "Infd"} {
		c := Defaults()
		c.StaleAfter = s
		d, err := c.StaleDuration()
		if err != nil || d < 100*365*24*time.Hour {
			t.Errorf("stale_after %q = %v, %v; want a huge positive duration", s, d, err)
		}
	}
}
