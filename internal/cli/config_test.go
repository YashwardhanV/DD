package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

func useConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	t.Setenv("DRIVEDEX_CONFIG", path)
	if content != "" {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestConfigPathPrintsTheFile(t *testing.T) {
	path := useConfigFile(t, "")
	out, err := run(t, "config", "path")
	if err != nil || strings.TrimSpace(out) != path {
		t.Errorf("err=%v out=%q want %q", err, out, path)
	}
}

func TestConfigIsCreatedWithDefaultsOnFirstRun(t *testing.T) {
	path := useConfigFile(t, "")
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	if _, err := run(t, "list"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config.toml should be created on first run: %v", err)
	}
	for _, want := range []string{"default_limit   = 50", "show_hidden     = true", `workers         = "auto"`, `stale_after     = "24h"`, `phone_root      = "/sdcard/"`, `color           = "auto"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("default file missing %q", want)
		}
	}
}

func TestConfigShowPrintsEffectiveValues(t *testing.T) {
	useConfigFile(t, "default_limit = 7\nworkers = 8\n")
	out, err := run(t, "config", "show")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"default_limit = 7", `workers = "8"`, "show_hidden = true", `phone_root = "/sdcard/"`} {
		if !strings.Contains(out, want) {
			t.Errorf("config show missing %q:\n%s", want, out)
		}
	}
}

func TestConfigShowNamesTheBrokenLine(t *testing.T) {
	useConfigFile(t, "show_hidden = true\ndefault_limit = \n")
	_, err := run(t, "config", "show")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "line 2") || !strings.Contains(ue.Hint, "drivedex config edit") {
		t.Errorf("err = %v", err)
	}
}

func TestBrokenConfigStopsNormalCommandsButNotTheOnesThatFixIt(t *testing.T) {
	useConfigFile(t, "color = \"rainbow\"\n")
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	_, err := run(t, "list")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "color must be") || !strings.Contains(ue.Hint, "config edit") {
		t.Errorf("list with a bad config: %v", err)
	}
	for _, args := range [][]string{{"config", "path"}, {"doctor"}, {"version"}, {"help"}} {
		if _, err := run(t, args...); err != nil {
			t.Errorf("%v must still work with a broken settings file: %v", args, err)
		}
	}
}

func TestConfigEditOpensTheFileAndCreatesIt(t *testing.T) {
	path := useConfigFile(t, "")
	var opened string
	old := openEditor
	openEditor = func(p string) error { opened = p; return nil }
	defer func() { openEditor = old }()

	out, err := run(t, "config", "edit")
	if err != nil || opened != path || !strings.Contains(out, "save the file") {
		t.Errorf("err=%v opened=%q out=%q", err, opened, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("edit should create the file first: %v", err)
	}

	// Notepad missing: the message says which file to open by hand.
	openEditor = func(string) error { return errors.ErrUnsupported }
	_, err = run(t, "config", "edit")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Hint, path) {
		t.Errorf("err = %v", err)
	}
}

func TestConfigEditWorksEvenIfTheFileIsBroken(t *testing.T) {
	path := useConfigFile(t, "this is not toml")
	opened := false
	old := openEditor
	openEditor = func(string) error { opened = true; return nil }
	defer func() { openEditor = old }()
	if _, err := run(t, "config", "edit"); err != nil || !opened {
		t.Errorf("the broken file is exactly what the user wants to edit: err=%v opened=%v", err, opened)
	}
	if b, _ := os.ReadFile(path); string(b) != "this is not toml" {
		t.Error("opening the editor must not rewrite the file")
	}
}

func TestStaleAfterFromConfigControlsTheRefreshTip(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))

	// Make the saved index two hours old.
	store, _ := openStore()
	ix, err := store.Load("vol-1A2B3C4D")
	if err != nil {
		t.Fatal(err)
	}
	ix.Header.UpdatedAt = time.Now().Add(-2 * time.Hour)
	store.Save(ix)

	useConfigFile(t, `stale_after = "24h"`+"\n")
	out, _ := run(t, "--no-color", "search", "resume", "--limit", "1")
	if strings.Contains(out, "tip:") {
		t.Errorf("2 h old is fresh under a 24 h setting:\n%s", out)
	}
	useConfigFile(t, `stale_after = "1h"`+"\n")
	out, _ = run(t, "--no-color", "search", "resume", "--limit", "1")
	if !strings.Contains(out, "tip: E:") || !strings.Contains(out, "run 'drivedex index E:' to refresh") {
		t.Errorf("2 h old is stale under a 1 h setting:\n%s", out)
	}
}

func TestAdbPathFromConfigIsUsed(t *testing.T) {
	useConfigFile(t, `adb_path = "C:\\does\\not\\exist\\adb.exe"`+"\n")
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	// Use the real finder (not the fake): a configured-but-missing adb is reported by name.
	_, err := run(t, "index", "phone")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(err.Error()+ue.Msg+ue.Hint+errString(ue.Err), "adb_path") {
		t.Errorf("a wrong adb_path should be called out, got: %v / %+v", err, ue)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestPhoneRootFromConfig(t *testing.T) {
	if got := (func() string {
		activeConfig.PhoneRoot = "/sdcard/DCIM"
		defer func() { activeConfig.PhoneRoot = "/sdcard/" }()
		return activePhoneRoot()
	})(); got != "/sdcard/DCIM/" {
		t.Errorf("activePhoneRoot = %q (a slash is added)", got)
	}
	activeConfig.PhoneRoot = "/sdcard/DCIM/"
	if got := activePhoneRootPath(); got != "/sdcard/DCIM" {
		t.Errorf("activePhoneRootPath = %q", got)
	}
	activeConfig.PhoneRoot = "/"
	if got := activePhoneRootPath(); got != "/" {
		t.Errorf("root path = %q", got)
	}
	activeConfig.PhoneRoot = "/sdcard/"
}

// NTFS turbo-mode entries have no size/date. A search that filters on them says so, once.
func TestMetadataFilterOnTurboEntriesPrintsTheTip(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	v := fakeDrive("E:")
	v.FileSystem = "NTFS"
	plug(t, v)

	ix := index.New()
	ix.Header = volumeHeader(v)
	ix.Header.Scanner = "mft"
	ix.Add(index.Entry{ID: 1, Name: "report.pdf", Size: -1, Flags: index.FlagNoMeta})
	ix.Add(index.Entry{ID: 2, Name: "report2.pdf", Size: -1, Flags: index.FlagNoMeta})
	ix.Header.UpdatedAt = time.Now()
	store, _ := openStore()
	if _, err := store.Save(ix); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd("test")
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--no-color", "search", "report", "size:>1kb"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "some NTFS turbo-mode entries have no size/date; run 'drivedex index E: --with-metadata' to include them."
	if n := strings.Count(stdout.String(), want); n != 1 {
		t.Errorf("the tip should be printed exactly once, got %d:\n%s", n, stdout.String())
	}
	// Without a size/date filter there's nothing to explain.
	out, _ := run(t, "--no-color", "search", "report")
	if strings.Contains(out, "turbo-mode entries have no size") {
		t.Errorf("no tip without a metadata filter:\n%s", out)
	}
}
