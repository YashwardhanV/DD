package install

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const dir = `C:\Users\Yash\AppData\Local\Programs\DriveDex`

func TestPathAdd(t *testing.T) {
	cases := []struct {
		name, in string
		want     string
		changed  bool
	}{
		{"empty", "", dir, true},
		{"blank", "  ", dir, true},
		{"normal", `C:\Windows;C:\Tools`, `C:\Windows;C:\Tools;` + dir, true},
		{"trailing semicolon is not doubled", `C:\Windows;`, `C:\Windows;` + dir, true},
		{"already there", `C:\Windows;` + dir, `C:\Windows;` + dir, false},
		{"already there, other case", `C:\Windows;` + strings.ToUpper(dir), `C:\Windows;` + strings.ToUpper(dir), false},
		{"already there, trailing slash", `C:\Windows;` + dir + `\`, `C:\Windows;` + dir + `\`, false},
		{"already there, quoted", `"` + dir + `";C:\Windows`, `"` + dir + `";C:\Windows`, false},
		{"already there, forward slashes", strings.ReplaceAll(dir, `\`, "/"), strings.ReplaceAll(dir, `\`, "/"), false},
		{"unexpanded variables are kept as typed", `%USERPROFILE%\bin;%SystemRoot%`, `%USERPROFILE%\bin;%SystemRoot%;` + dir, true},
		{"a folder with the same prefix is not a match", dir + `2`, dir + `2;` + dir, true},
	}
	for _, c := range cases {
		got, changed := PathAdd(c.in, dir)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: PathAdd(%q) = %q,%v want %q,%v", c.name, c.in, got, changed, c.want, c.changed)
		}
	}
}

func TestPathContainsExpandsVariables(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\Yash\AppData\Local`)
	if !PathContains(`C:\x;%LOCALAPPDATA%\Programs\DriveDex`, dir) {
		t.Error("an entry written with %LOCALAPPDATA% is the same folder")
	}
	if PathContains(`%NO_SUCH_VAR_XYZ%\Programs\DriveDex`, dir) {
		t.Error("an unknown variable must not match")
	}
}

func TestPathRemove(t *testing.T) {
	cases := []struct {
		name, in string
		want     string
		changed  bool
	}{
		{"last", `C:\Windows;` + dir, `C:\Windows`, true},
		{"first", dir + `;C:\Windows`, `C:\Windows`, true},
		{"middle", `C:\A;` + dir + `;C:\B`, `C:\A;C:\B`, true},
		{"only", dir, ``, true},
		{"not there", `C:\A;C:\B`, `C:\A;C:\B`, false},
		{"duplicates all go", dir + `;C:\A;` + strings.ToLower(dir), `C:\A`, true},
		{"other text is untouched, even odd spacing", ` C:\A ;` + dir + `;%X%\y`, ` C:\A ;%X%\y`, true},
		{"empty", ``, ``, false},
	}
	for _, c := range cases {
		got, changed := PathRemove(c.in, dir)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: PathRemove(%q) = %q,%v want %q,%v", c.name, c.in, got, changed, c.want, c.changed)
		}
	}
}

func TestAddThenRemoveRestoresThePathExactly(t *testing.T) {
	for _, original := range []string{``, `C:\Windows`, `C:\A;%USERPROFILE%\bin;C:\B`, `C:\A;`} {
		added, _ := PathAdd(original, dir)
		back, changed := PathRemove(added, dir)
		// A path that ended in ";" comes back without the empty trailing entry: same PATH, tidier.
		wantBack := strings.TrimSuffix(original, ";")
		if !changed || back != wantBack {
			t.Errorf("round trip of %q: added %q, removed %q (changed=%v)", original, added, back, changed)
		}
	}
}

type fakePath struct {
	value   string
	getErr  error
	setErr  error
	setCall int
}

func (f *fakePath) Get() (string, error) { return f.value, f.getErr }
func (f *fakePath) Set(v string) error {
	f.setCall++
	if f.setErr != nil {
		return f.setErr
	}
	f.value = v
	return nil
}

func TestAddToUserPath(t *testing.T) {
	fp := &fakePath{value: `C:\Windows`}
	changed, err := AddToUserPath(fp, dir)
	if err != nil || !changed || fp.value != `C:\Windows;`+dir {
		t.Fatalf("changed=%v err=%v value=%q", changed, err, fp.value)
	}
	// Idempotent: nothing is written the second time.
	changed, err = AddToUserPath(fp, dir)
	if err != nil || changed || fp.setCall != 1 {
		t.Errorf("second add: changed=%v err=%v writes=%d", changed, err, fp.setCall)
	}
	// Errors are explained and nothing is claimed.
	if _, err := AddToUserPath(&fakePath{getErr: errors.New("boom")}, dir); err == nil || !strings.Contains(err.Error(), "reading your PATH") {
		t.Errorf("get error: %v", err)
	}
	if changed, err := AddToUserPath(&fakePath{setErr: errors.New("denied")}, dir); err == nil || changed || !strings.Contains(err.Error(), "saving your PATH") {
		t.Errorf("set error: changed=%v %v", changed, err)
	}
}

func TestRemoveFromUserPath(t *testing.T) {
	fp := &fakePath{value: `C:\A;` + dir}
	if changed, err := RemoveFromUserPath(fp, dir); err != nil || !changed || fp.value != `C:\A` {
		t.Errorf("changed=%v err=%v value=%q", changed, err, fp.value)
	}
	if changed, err := RemoveFromUserPath(fp, dir); err != nil || changed || fp.setCall != 1 {
		t.Errorf("removing what isn't there must write nothing: changed=%v writes=%d", changed, fp.setCall)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, d string) []string {
	t.Helper()
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestCopyProgramCopiesAndReplaces(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "Programs", "DriveDex")
	write(t, filepath.Join(src, ExeName), "exe-v2")
	write(t, filepath.Join(src, AgentExeName), "agent-v2")
	write(t, filepath.Join(src, "other.txt"), "not copied")

	if err := CopyProgram(src, dst, ProgramFiles()); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dst); !reflect.DeepEqual(got, []string{AgentExeName, ExeName}) {
		t.Errorf("installed files = %v (no temp files, nothing extra)", got)
	}
	// Upgrade over an older install.
	write(t, filepath.Join(src, ExeName), "exe-v3")
	if err := CopyProgram(src, dst, ProgramFiles()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, ExeName)); string(b) != "exe-v3" {
		t.Errorf("exe = %q, want the new version", b)
	}
	// (Windows has no execute permission bit, so this only means something elsewhere.)
	if st, _ := os.Stat(filepath.Join(dst, ExeName)); runtime.GOOS != "windows" && st.Mode()&0o100 == 0 {
		t.Error("the program must stay executable")
	}
}

func TestCopyProgramFailureLeavesNoTempFilesAndKeepsTheOldProgram(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dst, ExeName), "old")
	// The agent is missing in the source: the copy fails part-way.
	write(t, filepath.Join(src, ExeName), "new")
	err := CopyProgram(src, dst, ProgramFiles())
	if err == nil || !strings.Contains(err.Error(), AgentExeName) {
		t.Fatalf("err = %v", err)
	}
	for _, n := range names(t, dst) {
		if strings.HasSuffix(n, ".new") {
			t.Errorf("temp file %s left behind", n)
		}
	}
}

func TestRemoveProgramOnlyRemovesItsOwnFilesAndEmptyFolder(t *testing.T) {
	d := filepath.Join(t.TempDir(), "DriveDex")
	write(t, filepath.Join(d, ExeName), "x")
	write(t, filepath.Join(d, AgentExeName), "x")
	write(t, filepath.Join(d, ExeName+".new"), "x")
	left, err := RemoveProgram(d)
	if err != nil || len(left) != 0 {
		t.Fatalf("left=%v err=%v", left, err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Error("an empty folder should be removed")
	}

	// A user's own file in the folder is never deleted - and then the folder stays.
	write(t, filepath.Join(d, ExeName), "x")
	write(t, filepath.Join(d, "my-notes.txt"), "precious")
	left, err = RemoveProgram(d)
	if err != nil || !reflect.DeepEqual(left, []string{"my-notes.txt"}) {
		t.Errorf("left=%v err=%v", left, err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "my-notes.txt")); string(b) != "precious" {
		t.Error("the user's file must survive")
	}
	if _, err := os.Stat(filepath.Join(d, ExeName)); !os.IsNotExist(err) {
		t.Error("drivedex.exe should still have been removed")
	}
}

func TestRemoveProgramOfMissingFolderIsFine(t *testing.T) {
	left, err := RemoveProgram(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(left) != 0 {
		t.Errorf("left=%v err=%v", left, err)
	}
	if ProgramPresent(filepath.Join(t.TempDir(), "nope")) {
		t.Error("nothing is present in a missing folder")
	}
}

func TestOwnedDataListsOnlyDriveDexFiles(t *testing.T) {
	root := t.TempDir()
	idx, data, cfg := filepath.Join(root, "indexes"), filepath.Join(root, "data"), filepath.Join(root, "cfg", "config.toml")
	write(t, filepath.Join(idx, "vol-1.ddx"), "12345")
	write(t, filepath.Join(idx, "vol-1.lock"), "")
	write(t, filepath.Join(idx, "vol-2.ddx.tmp"), "")
	write(t, filepath.Join(idx, "holiday-photos.jpg"), "NOT ours")
	write(t, filepath.Join(idx, "notes.txt"), "NOT ours")
	write(t, filepath.Join(data, "agent.log"), "log")
	write(t, filepath.Join(data, "agent.pid"), "1")
	write(t, filepath.Join(data, "other.dat"), "NOT ours")
	write(t, cfg, "default_limit = 5")

	files := OwnedData(idx, data, cfg)
	var base []string
	var total int64
	for _, f := range files {
		base = append(base, filepath.Base(f.Path))
		total += f.Size
	}
	sort.Strings(base)
	want := []string{"agent.log", "agent.pid", "config.toml", "vol-1.ddx", "vol-1.lock", "vol-2.ddx.tmp"}
	if !reflect.DeepEqual(base, want) {
		t.Errorf("owned files = %v, want %v", base, want)
	}
	if total != int64(5+3+1+len("default_limit = 5")) {
		t.Errorf("total size = %d", total)
	}

	if err := RemoveOwnedData(files, idx, data, filepath.Dir(cfg)); err != nil {
		t.Fatal(err)
	}
	// The folders that held only DriveDex's files are gone; the ones with other files stay with them.
	if _, err := os.Stat(filepath.Dir(cfg)); !os.IsNotExist(err) {
		t.Error("the now-empty settings folder should be removed")
	}
	if got := names(t, idx); !reflect.DeepEqual(got, []string{"holiday-photos.jpg", "notes.txt"}) {
		t.Errorf("foreign files in the index folder must survive: %v", got)
	}
	if got := names(t, data); !reflect.DeepEqual(got, []string{"other.dat"}) {
		t.Errorf("foreign files in the data folder must survive: %v", got)
	}
}

func TestOwnedDataOfMissingFolders(t *testing.T) {
	if got := OwnedData(filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b"), filepath.Join(t.TempDir(), "c.toml")); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestRemovalCommandLine(t *testing.T) {
	d := filepath.Join(t.TempDir(), "DriveDex")
	a, b := filepath.Join(d, ExeName), filepath.Join(d, AgentExeName)
	line, err := removalCommandLine(d, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := `cmd.exe /d /s /c "ping -n 4 127.0.0.1 >nul & del /f /q "` + a + `" "` + b + `" & rmdir "` + d + `""`
	if line != want {
		t.Errorf("line =\n%s\nwant\n%s", line, want)
	}
	if strings.Contains(line, "/s \"") && strings.Contains(line, "rmdir /s") {
		t.Error("never rmdir /s: only an empty folder may be removed")
	}
}

func TestRemovalCommandLineRefusesDangerousInput(t *testing.T) {
	bad := []struct{ dir, path string }{
		{"", ""},
		{"/", "/x"}, // a drive root / filesystem root
		{"relative/dir", "relative/dir/x"},
		{"/tmp/100%/DriveDex", "/tmp/100%/DriveDex/drivedex.exe"}, // % would be expanded by cmd
		{"/tmp/a\"b", "/tmp/a\"b/x"},
		{"/tmp/ok", "/tmp/ok/line\nbreak"},
	}
	for _, c := range bad {
		if line, err := removalCommandLine(c.dir, []string{c.path}); err == nil {
			t.Errorf("dir %q path %q should be refused, got %s", c.dir, c.path, line)
		}
	}
}

func TestIsInUse(t *testing.T) {
	for _, s := range []string{"rename a b: Access is denied.", "The process cannot access the file because it is being used by another process.", "permission denied"} {
		if !IsInUse(errors.New(s)) {
			t.Errorf("%q should look like 'in use'", s)
		}
	}
	if IsInUse(nil) || IsInUse(errors.New("disk full")) {
		t.Error("false positives")
	}
}
