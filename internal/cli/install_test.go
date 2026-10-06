package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/install"
)

type memPath struct {
	value string
	err   error
	sets  int
}

func (m *memPath) Get() (string, error) { return m.value, m.err }
func (m *memPath) Set(v string) error   { m.sets++; m.value = v; return nil }

// installWorld is a pretend PC: a "downloaded" release folder, an install
// folder, a user PATH, a registry and an agent.
type installWorld struct {
	t       *testing.T
	src     string // where the downloaded drivedex.exe lives
	dir     string // the install folder
	path    *memPath
	reg     *memShell
	agent   *agentEnv
	removed []string // what scheduleRemoval was asked to delete
	running string   // directory of the "running" program (src unless a test moves it)
}

func newInstallWorld(t *testing.T) *installWorld {
	t.Helper()
	w := &installWorld{t: t, src: t.TempDir(), dir: filepath.Join(t.TempDir(), "Programs", "DriveDex")}
	for _, n := range install.ProgramFiles() {
		if err := os.WriteFile(filepath.Join(w.src, n), []byte("binary:"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DRIVEDEX_INSTALL_DIR", w.dir)
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	t.Setenv("DRIVEDEX_CONFIG", filepath.Join(t.TempDir(), "cfg", "config.toml"))
	w.path = &memPath{value: `C:\Windows;%USERPROFILE%\bin`}
	w.reg = useMemShell(t)
	w.agent = newAgentEnv(t)
	t.Setenv("DRIVEDEX_DATA_DIR", w.agent.dir)
	w.running = w.src
	shellExe = func() (string, error) { return filepath.Join(w.running, install.ExeName), nil }

	oldPS, oldRM := newPathStore, scheduleRemoval
	newPathStore = func() install.PathStore { return w.path }
	scheduleRemoval = func(dir string, names []string) error { w.removed = append(w.removed, names...); return nil }
	t.Cleanup(func() { newPathStore, scheduleRemoval = oldPS, oldRM })
	plug(t) // doctor looks at drives
	return w
}

func (w *installWorld) install(stdin string, args ...string) (string, error) {
	w.t.Helper()
	root := NewRootCmd("1.2.3")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--no-color", "install"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func (w *installWorld) uninstall(stdin string, args ...string) (string, error) {
	w.t.Helper()
	root := NewRootCmd("1.2.3")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--no-color", "uninstall"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestInstallAsksBeforeEachChangeAndDoesEverythingOnYes(t *testing.T) {
	w := newInstallWorld(t)
	out, err := w.install("y\ny\ny\ny\n") // copy, PATH, menu, start-up
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Step 1 of 5", "Copy drivedex.exe and drivedex-agent.exe into " + w.dir, "[y/N]",
		"Step 2 of 5", "Add " + w.dir + " to your user PATH", "NEW terminal",
		"Step 3 of 5", "Search with DriveDex", "Done. Right-click",
		"Step 4 of 5", "starts every time you log in",
		"Step 5 of 5", "version 1.2.3",
		"DriveDex is installed", "drivedex uninstall",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The doctor run at the end must see the freshly installed menu as fine, even though
	// the running program is the downloaded copy, not the installed one.
	if !strings.Contains(out, "right-click menu is installed") || strings.Contains(out, "partly installed") {
		t.Errorf("doctor should report the right-click menu as installed:\n%s", out)
	}
	// 1. files
	for _, n := range install.ProgramFiles() {
		b, err := os.ReadFile(filepath.Join(w.dir, n))
		if err != nil || string(b) != "binary:"+n {
			t.Errorf("%s not installed correctly: %q %v", n, b, err)
		}
	}
	// 2. PATH: old entries kept verbatim, folder appended.
	if w.path.value != `C:\Windows;%USERPROFILE%\bin;`+w.dir {
		t.Errorf("PATH = %q", w.path.value)
	}
	// 3. right-click entries point at the INSTALLED program, not at the download.
	found := false
	for _, vals := range w.reg.keys {
		for _, v := range vals {
			if strings.Contains(v, install.ExeName) {
				found = true
				if !strings.Contains(v, w.dir) || strings.Contains(v, w.src) {
					t.Errorf("menu entry %q must run the installed copy in %s", v, w.dir)
				}
			}
		}
	}
	if !found {
		t.Error("no right-click entries were written")
	}
	// 4. start-up entry points at the installed agent.
	if got := w.agent.reg[agent.RunValueName]; !strings.Contains(got, filepath.Join(w.dir, install.AgentExeName)) {
		t.Errorf("start-up entry = %q", got)
	}
}

func TestInstallAnswersNoMeansNothingChanges(t *testing.T) {
	w := newInstallWorld(t)
	out, err := w.install("n\n")
	if err != nil || !strings.Contains(out, "Cancelled. Nothing was changed.") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if exists(w.dir) || w.path.sets != 0 || len(w.reg.keys) != 0 || len(w.agent.reg) != 0 {
		t.Errorf("declining the first question must change nothing: dir=%v pathWrites=%d keys=%d", exists(w.dir), w.path.sets, len(w.reg.keys))
	}
}

func TestInstallEachStepCanBeDeclinedSeparately(t *testing.T) {
	w := newInstallWorld(t)
	out, err := w.install("y\nn\nn\nn\n") // copy yes; PATH, menu, start-up no
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(filepath.Join(w.dir, install.ExeName)) {
		t.Error("the copy was agreed to")
	}
	if w.path.sets != 0 || len(w.reg.keys) != 0 || len(w.agent.reg) != 0 {
		t.Errorf("declined steps must not change anything: PATH writes=%d keys=%d startup=%v", w.path.sets, len(w.reg.keys), w.agent.reg)
	}
	if !strings.Contains(out, "Skipped. You can still run it with its full path:") || !strings.Contains(out, filepath.Join(w.dir, install.ExeName)) {
		t.Errorf("when PATH is declined, say how to run it:\n%s", out)
	}
}

func TestInstallYesAnswersEveryQuestion(t *testing.T) {
	w := newInstallWorld(t)
	out, err := w.install("", "--yes")
	if err != nil || strings.Contains(out, "[y/N]") {
		t.Fatalf("--yes must not ask: err=%v\n%s", err, out)
	}
	if !exists(filepath.Join(w.dir, install.AgentExeName)) || !strings.Contains(w.path.value, w.dir) || len(w.reg.keys) == 0 || w.agent.reg[agent.RunValueName] == "" {
		t.Error("--yes should do every step")
	}
}

func TestInstallIsRepeatableAndRepairs(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	writesAfterFirst := w.path.sets
	// A newer download is installed over the old one; PATH is not touched again.
	os.WriteFile(filepath.Join(w.src, install.ExeName), []byte("binary:v2"), 0o755)
	out, err := w.install("", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, install.ExeName)); string(b) != "binary:v2" {
		t.Errorf("upgrade didn't replace the program: %q", b)
	}
	if w.path.sets != writesAfterFirst || !strings.Contains(out, "is already on your PATH") {
		t.Errorf("PATH must not be written twice (writes %d -> %d):\n%s", writesAfterFirst, w.path.sets, out)
	}
	if n := strings.Count(w.path.value, w.dir); n != 1 {
		t.Errorf("the folder appears %d times in PATH: %q", n, w.path.value)
	}
}

func TestInstallFromTheInstalledCopySkipsTheCopy(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	w.running = w.dir
	out, err := w.install("", "--yes")
	if err != nil || !strings.Contains(out, "already the installed copy") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestInstallStopsAndRestartsARunningAgentAroundTheUpgrade(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	w.agent.runAgent(777) // the installed agent is running
	w.agent.started = nil
	out, err := w.install("y\ny\ny\ny\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "it will be stopped for the copy and started again") {
		t.Errorf("the question must warn about the agent:\n%s", out)
	}
	if len(w.agent.killed) != 1 || w.agent.killed[0] != 777 {
		t.Errorf("the old agent should be stopped first: killed=%v", w.agent.killed)
	}
	if len(w.agent.started) != 1 || w.agent.started[0] != filepath.Join(w.dir, install.AgentExeName) {
		t.Errorf("the INSTALLED agent should be started afterwards (not the downloaded one): %v", w.agent.started)
	}
}

func TestInstallProblems(t *testing.T) {
	t.Run("agent missing from the download", func(t *testing.T) {
		w := newInstallWorld(t)
		os.Remove(filepath.Join(w.src, install.AgentExeName))
		_, err := w.install("", "--yes")
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, install.AgentExeName) || exists(w.dir) {
			t.Errorf("err=%v dirCreated=%v", err, exists(w.dir))
		}
	})
	t.Run("run from a renamed exe", func(t *testing.T) {
		w := newInstallWorld(t)
		shellExe = func() (string, error) { return filepath.Join(w.src, "dd-test.exe"), nil }
		_, err := w.install("", "--yes")
		if err == nil || !strings.Contains(userMsg(err), "must be run from drivedex.exe") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("PATH can't be saved", func(t *testing.T) {
		w := newInstallWorld(t)
		w.path.err = errors.New("registry locked")
		_, err := w.install("", "--yes")
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Hint, "Environment Variables") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("not on Windows", func(t *testing.T) {
		w := newInstallWorld(t)
		newPathStore = func() install.PathStore { return install.WindowsUserPath{} }
		if runtimeIsWindows() {
			t.Skip()
		}
		_, err := w.install("", "--yes")
		if err == nil || !strings.Contains(userMsg(err), "only works on Windows") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no install folder known", func(t *testing.T) {
		newInstallWorld(t)
		old := installDirFor
		installDirFor = func() (string, error) { return "", errors.New("%LOCALAPPDATA% isn't set") }
		defer func() { installDirFor = old }()
		_, err := run(t, "install", "--yes")
		if err == nil || !strings.Contains(userMsg(err), "LOCALAPPDATA") {
			t.Errorf("err = %v", err)
		}
	})
}

func runtimeIsWindows() bool { return os.PathSeparator == '\\' }

func TestUninstallReversesEverythingAndKeepsData(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	w.agent.runAgent(555)
	idx := filepath.Join(os.Getenv("DRIVEDEX_INDEX_DIR"), "vol-1.ddx")
	os.MkdirAll(filepath.Dir(idx), 0o755)
	os.WriteFile(idx, []byte("index"), 0o644)
	cfg := os.Getenv("DRIVEDEX_CONFIG")

	out, err := w.uninstall("y\ny\ny\ny\nn\n") // menu, agent, PATH, folder yes; data no
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Step 1 of 5", "Remove the 4 DriveDex right-click menu entries", "Step 5 of 5", "Delete them too?", "Kept.", "Files on your drives were never touched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(w.reg.keys) != 0 {
		t.Errorf("right-click keys left: %v", w.reg.keys)
	}
	if len(w.agent.killed) != 1 || w.agent.reg[agent.RunValueName] != "" {
		t.Errorf("agent should be stopped and removed from start-up: killed=%v reg=%v", w.agent.killed, w.agent.reg)
	}
	if w.path.value != `C:\Windows;%USERPROFILE%\bin` {
		t.Errorf("PATH must be exactly as before install: %q", w.path.value)
	}
	if install.ProgramPresent(w.dir) || exists(w.dir) {
		t.Errorf("program folder should be gone")
	}
	if !exists(idx) || !exists(cfg) {
		t.Error("indexes and settings must be kept unless the user says delete")
	}
}

func TestUninstallCanDeleteDataWhenAsked(t *testing.T) {
	w := newInstallWorld(t)
	idxDir := os.Getenv("DRIVEDEX_INDEX_DIR")
	os.MkdirAll(idxDir, 0o755)
	os.WriteFile(filepath.Join(idxDir, "vol-1.ddx"), []byte("index"), 0o644)
	os.WriteFile(filepath.Join(idxDir, "my-own-notes.txt"), []byte("mine"), 0o644)
	os.MkdirAll(filepath.Dir(os.Getenv("DRIVEDEX_CONFIG")), 0o755)
	os.WriteFile(os.Getenv("DRIVEDEX_CONFIG"), []byte("default_limit = 5"), 0o644)

	out, err := w.uninstall("", "--yes", "--purge")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(filepath.Join(idxDir, "vol-1.ddx")) || exists(os.Getenv("DRIVEDEX_CONFIG")) {
		t.Errorf("--purge should delete DriveDex's own data:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(idxDir, "my-own-notes.txt")); string(b) != "mine" {
		t.Error("a file that isn't DriveDex's own must never be deleted")
	}
}

func TestUninstallYesStillKeepsYourDataWithoutPurge(t *testing.T) {
	w := newInstallWorld(t)
	idxDir := os.Getenv("DRIVEDEX_INDEX_DIR")
	os.MkdirAll(idxDir, 0o755)
	os.WriteFile(filepath.Join(idxDir, "vol-1.ddx"), []byte("index"), 0o644)
	out, err := w.uninstall("", "--yes")
	if err != nil || !exists(filepath.Join(idxDir, "vol-1.ddx")) {
		t.Errorf("--yes must not delete the user's indexes (err=%v):\n%s", err, out)
	}
	if !strings.Contains(out, "Delete them too?") || !strings.Contains(out, "Kept.") {
		t.Errorf("should still ask about the data:\n%s", out)
	}
}

func TestUninstallAnswerYesToDataDeletesIt(t *testing.T) {
	w := newInstallWorld(t)
	idxDir := os.Getenv("DRIVEDEX_INDEX_DIR")
	os.MkdirAll(idxDir, 0o755)
	os.WriteFile(filepath.Join(idxDir, "vol-1.ddx"), []byte("index"), 0o644)
	// Nothing else is installed, so the only question asked is the last one.
	out, err := w.uninstall("y\n")
	if err != nil || exists(filepath.Join(idxDir, "vol-1.ddx")) || !strings.Contains(out, "Deleted.") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestUninstallEachQuestionCanBeDeclined(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, err := w.uninstall("n\nn\nn\nn\nn\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(w.reg.keys) == 0 || w.agent.reg[agent.RunValueName] == "" || !strings.Contains(w.path.value, w.dir) || !install.ProgramPresent(w.dir) {
		t.Errorf("declining must keep everything:\n%s", out)
	}
}

// The running program can't delete itself on Windows: it asks a helper to do it
// after it exits - for exactly its own files, nothing more.
func TestUninstallFromTheInstalledCopySchedulesTheRemoval(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	w.running = w.dir
	out, err := w.uninstall("", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Scheduled.") {
		t.Errorf("output:\n%s", out)
	}
	for _, n := range w.removed {
		if n != install.ExeName && n != install.AgentExeName && n != install.ExeName+".old" {
			t.Errorf("only DriveDex's own files may be scheduled for removal, got %q", n)
		}
	}
	if len(w.removed) < 2 {
		t.Errorf("removed = %v", w.removed)
	}
	// Nothing was deleted synchronously (the helper does it later).
	if !install.ProgramPresent(w.dir) {
		t.Error("the files are removed by the helper after exit, not before")
	}
}

func TestUninstallLeavesAFolderWithOtherFiles(t *testing.T) {
	w := newInstallWorld(t)
	if _, err := w.install("", "--yes"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.dir, "my-notes.txt"), []byte("mine"), 0o644)
	out, err := w.uninstall("", "--yes")
	if err != nil || !strings.Contains(out, "was left in place because it contains other files: my-notes.txt") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "my-notes.txt")); string(b) != "mine" {
		t.Error("the user's file must survive")
	}
}

func TestUninstallWhenNothingIsInstalledSaysSo(t *testing.T) {
	w := newInstallWorld(t)
	out, err := w.uninstall("")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Not installed.", "not running and not set to start", "isn't on your PATH", "No installed program found", "No saved indexes or settings found"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInstallAndUninstallWorkWithABrokenConfigFile(t *testing.T) {
	w := newInstallWorld(t)
	cfg := os.Getenv("DRIVEDEX_CONFIG")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte("color = 'purple'"), 0o644)
	if _, err := w.install("", "--yes"); err != nil {
		t.Errorf("install must work with a broken config: %v", err)
	}
	if _, err := w.uninstall("", "--yes"); err != nil {
		t.Errorf("uninstall must work with a broken config: %v", err)
	}
}
