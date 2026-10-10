package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/shellint"
)

// memShell is an in-memory registry for the shell commands.
type memShell struct{ keys map[string]map[string]string }

func (m *memShell) SetValue(k, n, v string) error {
	if m.keys[k] == nil {
		m.keys[k] = map[string]string{}
	}
	m.keys[k][n] = v
	return nil
}
func (m *memShell) GetValue(k, n string) (string, bool, error) {
	v, ok := m.keys[k][n]
	return v, ok, nil
}
func (m *memShell) DeleteTree(k string) (bool, error) {
	removed := false
	for key := range m.keys {
		if key == k || strings.HasPrefix(key, k+`\`) {
			delete(m.keys, key)
			removed = true
		}
	}
	return removed, nil
}

func useMemShell(t *testing.T) *memShell {
	t.Helper()
	m := &memShell{keys: map[string]map[string]string{}}
	oldReg, oldExe := newShellRegistry, shellExe
	newShellRegistry = func() shellint.Registry { return m }
	shellExe = func() (string, error) { return `C:\Program Files\DriveDex\drivedex.exe`, nil }
	t.Cleanup(func() { newShellRegistry, shellExe = oldReg, oldExe })
	return m
}

func TestShellInstallAsksFirstThenWritesTheKeys(t *testing.T) {
	m := useMemShell(t)

	var out bytes.Buffer
	if err := runShellInstall(&out, strings.NewReader("n\n"), false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search with DriveDex", "Index with DriveDex", `Directory\Background\shell\DriveDex`, `Drive\shell\DriveDexIndex`, "HKEY_CURRENT_USER", "Continue? [y/N]", "Cancelled"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the question must show %q:\n%s", want, out.String())
		}
	}
	if len(m.keys) != 0 {
		t.Error("declining must write nothing")
	}

	out.Reset()
	if err := runShellInstall(&out, strings.NewReader("y\n"), false); err != nil {
		t.Fatal(err)
	}
	if len(m.keys) != 8 || !strings.Contains(out.String(), "Done.") || !strings.Contains(out.String(), "Show more options") {
		t.Errorf("keys=%d\n%s", len(m.keys), out.String())
	}
}

func TestShellStatusAndUninstall(t *testing.T) {
	m := useMemShell(t)

	out, err := run(t, "shell", "status")
	if err != nil || !strings.Contains(out, "Not installed") || strings.Count(out, "missing") != 4 {
		t.Errorf("before install: %v\n%s", err, out)
	}

	if _, err := run(t, "shell", "install", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "shell", "status")
	if strings.Count(out, "installed") < 4 || !strings.Contains(out, "All entries are installed") {
		t.Errorf("after install:\n%s", out)
	}

	// The program moved: entries are out of date, and the status says how to fix it.
	shellExe = func() (string, error) { return `D:\Moved\drivedex.exe`, nil }
	out, _ = run(t, "shell", "status")
	if !strings.Contains(out, "points to a different drivedex.exe") || !strings.Contains(out, "drivedex shell install") {
		t.Errorf("stale:\n%s", out)
	}

	out, err = run(t, "shell", "uninstall")
	if err != nil || !strings.Contains(out, "Removed 4 right-click menu entries") || len(m.keys) != 0 {
		t.Errorf("uninstall: %v keys=%d\n%s", err, len(m.keys), out)
	}
	out, _ = run(t, "shell", "uninstall")
	if !strings.Contains(out, "Nothing to remove") {
		t.Errorf("second uninstall:\n%s", out)
	}
}

func TestShellUninstallReportsAForeignKey(t *testing.T) {
	m := useMemShell(t)
	m.SetValue(`Software\Classes\Drive\shell\DriveDex`, "", "Some other tool")
	m.SetValue(`Software\Classes\Drive\shell\DriveDex\command`, "", `"C:\other.exe"`)
	out, _ := run(t, "shell", "uninstall")
	if !strings.Contains(out, "left Drive") || !strings.Contains(out, "isn't a DriveDex entry") || len(m.keys) != 2 {
		t.Errorf("keys=%d\n%s", len(m.keys), out)
	}
}

func TestShellUnsupportedPlatform(t *testing.T) {
	oldReg := newShellRegistry
	newShellRegistry = func() shellint.Registry { return shellint.WindowsRegistry{} }
	defer func() { newShellRegistry = oldReg }()
	if os.PathSeparator == '\\' {
		t.Skip("on Windows the real registry is used")
	}
	for _, args := range [][]string{{"shell", "install", "--yes"}, {"shell", "uninstall"}, {"shell", "status"}} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "only works on Windows") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// ---- --pause ---------------------------------------------------------------------------

func TestPauseWaitsForEnterAfterTheCommand(t *testing.T) {
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version", "--pause"})
	code := executeRoot(context.Background(), root, strings.NewReader("\n"), &out)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	text := out.String()
	vi, pi := strings.Index(text, "drivedex test"), strings.Index(text, "Press Enter to close…")
	if vi < 0 || pi < 0 || pi < vi {
		t.Errorf("the pause prompt must come AFTER the command's output:\n%s", text)
	}
}

func TestPauseHappensEvenWhenTheCommandFails(t *testing.T) {
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"index", "/definitely/not/here", "--pause"})
	code := executeRoot(context.Background(), root, strings.NewReader("\n"), &out)
	if code != 1 || !strings.Contains(out.String(), "Press Enter to close…") {
		t.Errorf("code=%d\n%s", code, out.String())
	}
}

func TestNoPauseWithoutTheFlag(t *testing.T) {
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	executeRoot(context.Background(), root, strings.NewReader(""), &out)
	if strings.Contains(out.String(), "Press Enter") {
		t.Errorf("no --pause, no prompt:\n%s", out.String())
	}
}

func TestPauseReturnsOnEndOfInput(t *testing.T) {
	var out bytes.Buffer
	waitForEnter(strings.NewReader(""), &out) // stdin closed: must not hang
	if !strings.Contains(out.String(), "Press Enter to close…") {
		t.Errorf("out=%q", out.String())
	}
}

func TestRightClickPathsWithATrailingSpaceAreAccepted(t *testing.T) {
	// The menu passes `"E:\ "` (see shellint); every path argument must tolerate the space.
	tree, _ := withFixture(t)
	if _, err := run(t, "index", tree+" "); err != nil {
		t.Errorf("index with a trailing space: %v", err)
	}
	var ue *UserError
	_, err := resolveRoot(missingDrive(t) + `\ `)
	if err == nil || !errors.As(err, &ue) {
		t.Errorf("a missing drive is still a friendly error: %v", err)
	}
}

// missingDrive returns a drive letter such as "Q:" that isn't in use on this PC,
// so tests about unplugged drives don't break on a PC that really has an E:.
func missingDrive(t *testing.T) string {
	t.Helper()
	for c := 'Z'; c >= 'D'; c-- {
		d := string(c) + ":"
		if _, err := os.Stat(d + `\`); err != nil {
			return d
		}
	}
	t.Skip("every drive letter from D: to Z: is in use")
	return ""
}
