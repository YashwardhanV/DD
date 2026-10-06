package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

// helperCommand makes ChildIndexer run THIS test binary (as TestHelperIndex)
// instead of the real drivedex.exe; it works the same on every OS.
func helperCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cs := append([]string{"-test.run=^TestHelperIndex$", "--", name}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], cs...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_INDEX=1")
	return cmd
}

// TestHelperIndex is the stand-in `drivedex index <arg>`.
func TestHelperIndex(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_INDEX") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// args: <exe> index <arg>
	switch os.Getenv("HELPER_MODE") {
	case "fail":
		fmt.Fprintln(os.Stderr, "Error: can't open E:")
		fmt.Fprintln(os.Stderr, "  Fix: is the drive plugged in?")
		os.Exit(1)
	case "nosave":
		os.Exit(0) // claims success but writes nothing
	}
	ix := index.New()
	ix.Header = index.Header{SourceKind: index.KindVolume, SourceID: os.Getenv("HELPER_ID"), Label: "TEST"}
	for i := 0; i < 1234; i++ {
		ix.Add(index.Entry{ID: uint64(i + 1), Name: fmt.Sprintf("f%d", i)})
	}
	store, _ := index.OpenDefault()
	if _, err := store.Save(ix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("Indexed", args[len(args)-1])
	os.Exit(0)
}

func withHelper(t *testing.T, mode string) *index.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DRIVEDEX_INDEX_DIR", dir)
	t.Setenv("HELPER_MODE", mode)
	t.Setenv("HELPER_ID", "vol-1A2B3C4D")
	oldCmd, oldFind := commandFor, findDrivedex
	commandFor = helperCommand
	findDrivedex = func() (string, error) { return "drivedex", nil }
	t.Cleanup(func() { commandFor, findDrivedex = oldCmd, oldFind })
	return index.NewStore(dir)
}

func TestChildIndexerRunsTheCLIAndReadsTheEntryCount(t *testing.T) {
	store := withHelper(t, "")
	got, err := ChildIndexer(store)(context.Background(), target())
	if err != nil || got.Entries != 1234 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestChildIndexerReportsFailuresWithTheLastOutputLine(t *testing.T) {
	store := withHelper(t, "fail")
	_, err := ChildIndexer(store)(context.Background(), target())
	if err == nil || !strings.Contains(err.Error(), "`drivedex index E:` failed") || !strings.Contains(err.Error(), "Fix: is the drive plugged in?") {
		t.Errorf("err = %v", err)
	}
}

func TestChildIndexerNoticesAMissingIndex(t *testing.T) {
	store := withHelper(t, "nosave")
	if _, err := ChildIndexer(store)(context.Background(), target()); err == nil || !strings.Contains(err.Error(), "wasn't saved") {
		t.Errorf("err = %v", err)
	}
}

func TestChildIndexerWithoutDrivedex(t *testing.T) {
	old := findDrivedex
	findDrivedex = func() (string, error) { return "", errors.New("drivedex.exe isn't installed") }
	defer func() { findDrivedex = old }()
	if _, err := ChildIndexer(index.NewStore(t.TempDir()))(context.Background(), target()); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("err = %v", err)
	}
}

func TestChildIndexerStopsWhenCancelled(t *testing.T) {
	store := withHelper(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ChildIndexer(store)(ctx, target()); err == nil {
		t.Error("a cancelled agent must not report success")
	}
}

// ---- paths and PID file ------------------------------------------------------------

func TestDataDirAndPaths(t *testing.T) {
	t.Setenv(EnvDataDir, "/somewhere/DriveDex")
	d, err := DataDir()
	if err != nil || d != "/somewhere/DriveDex" {
		t.Errorf("DataDir = %q %v", d, err)
	}
	if LogPath(d) != filepath.Join(d, "agent.log") || PIDPath(d) != filepath.Join(d, "agent.pid") {
		t.Error("log and pid file names are fixed by the spec")
	}
	t.Setenv(EnvDataDir, "")
	if d, err := DataDir(); err != nil || !strings.HasSuffix(d, "DriveDex") {
		t.Errorf("default DataDir = %q %v", d, err)
	}
}

func TestPIDFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := ReadPID(dir); !errors.Is(err, ErrNoPID) {
		t.Errorf("no file: %v", err)
	}
	if err := WritePID(dir); err != nil {
		t.Fatal(err)
	}
	info, err := ReadPID(dir)
	if err != nil || info.PID != os.Getpid() || info.Started.IsZero() {
		t.Fatalf("info = %+v %v", info, err)
	}
	if got, running := Running(dir); !running || got.PID != os.Getpid() {
		t.Errorf("this very process is running: %+v %v", got, running)
	}
	RemovePID(dir)
	if _, running := Running(dir); running {
		t.Error("no PID file means not running")
	}

	for _, bad := range []string{"", "notanumber\n", "-5\n", "0\n"} {
		os.WriteFile(PIDPath(dir), []byte(bad), 0o644)
		if _, err := ReadPID(dir); err == nil || errors.Is(err, ErrNoPID) {
			t.Errorf("%q should be reported as a damaged PID file, got %v", bad, err)
		}
	}
	// A PID of a process that no longer exists (a crash left the file behind).
	os.WriteFile(PIDPath(dir), []byte("2147483646\n2026-01-01T00:00:00Z\n"), 0o644)
	if _, running := Running(dir); running {
		t.Error("a PID file for a dead process is not a running agent")
	}
}

func TestSiblingExe(t *testing.T) {
	if _, err := SiblingExe("definitely-not-installed"); err == nil || !strings.Contains(err.Error(), "was not found next to") {
		t.Errorf("err = %v", err)
	}
	self, _ := os.Executable()
	name := strings.TrimSuffix(filepath.Base(self), exeSuffix)
	if p, err := SiblingExe(name); err != nil || filepath.Base(p) != filepath.Base(self) {
		t.Errorf("a program next to us must be found: %q %v", p, err)
	}
}

// ---- start-up registration ------------------------------------------------------------

type fakeReg struct{ vals map[string]string }

func (r *fakeReg) Get(n string) (string, bool, error) { v, ok := r.vals[n]; return v, ok, nil }
func (r *fakeReg) Set(n, v string) error              { r.vals[n] = v; return nil }
func (r *fakeReg) Delete(n string) (bool, error) {
	_, ok := r.vals[n]
	delete(r.vals, n)
	return ok, nil
}

type fakeTasks struct {
	tasks   map[string]string
	needsAd bool
}

func (f *fakeTasks) Create(n, c string) error {
	if f.needsAd {
		return ErrNeedAdminForTask
	}
	f.tasks[n] = c
	return nil
}
func (f *fakeTasks) Exists(n string) (bool, error) { _, ok := f.tasks[n]; return ok, nil }
func (f *fakeTasks) Delete(n string) (bool, error) {
	_, ok := f.tasks[n]
	delete(f.tasks, n)
	return ok, nil
}

func newStartupFakes() (*fakeReg, *fakeTasks) {
	return &fakeReg{vals: map[string]string{}}, &fakeTasks{tasks: map[string]string{}}
}

const agentPath = `C:\Program Files\DriveDex\drivedex-agent.exe`

func TestEnableWritesTheRunValueWithQuotes(t *testing.T) {
	reg, tasks := newStartupFakes()
	if err := Enable(reg, tasks, agentPath, false); err != nil {
		t.Fatal(err)
	}
	if got := reg.vals[RunValueName]; got != `"`+agentPath+`"` {
		t.Errorf("Run value = %q (the path must be quoted so spaces work)", got)
	}
	st, _ := Status(reg, tasks)
	if !st.Enabled() || st.Elevated() || !strings.Contains(st.Describe(), "starts at log-in") {
		t.Errorf("status = %+v / %s", st, st.Describe())
	}
}

func TestElevatedEnableUsesTheTaskAndRemovesTheRunValue(t *testing.T) {
	reg, tasks := newStartupFakes()
	Enable(reg, tasks, agentPath, false) // previously the plain way
	if err := Enable(reg, tasks, agentPath, true); err != nil {
		t.Fatal(err)
	}
	if tasks.tasks[TaskName] != `"`+agentPath+`"` {
		t.Errorf("task command = %q", tasks.tasks[TaskName])
	}
	if _, ok := reg.vals[RunValueName]; ok {
		t.Error("only ONE start-up method may be active, or two agents race at log-in")
	}
	st, _ := Status(reg, tasks)
	if !st.Elevated() || !strings.Contains(st.Describe(), "administrator") {
		t.Errorf("status = %+v / %s", st, st.Describe())
	}
	// And switching back removes the task.
	if err := Enable(reg, tasks, agentPath, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := tasks.tasks[TaskName]; ok || reg.vals[RunValueName] == "" {
		t.Error("switching to the normal start-up must remove the elevated task")
	}
}

func TestElevatedEnableNeedsAdmin(t *testing.T) {
	reg, tasks := newStartupFakes()
	tasks.needsAd = true
	if err := Enable(reg, tasks, agentPath, true); !errors.Is(err, ErrNeedAdminForTask) {
		t.Errorf("err = %v", err)
	}
	if len(reg.vals) != 0 {
		t.Error("a failed elevated enable must not leave a Run value behind")
	}
}

func TestDisable(t *testing.T) {
	reg, tasks := newStartupFakes()
	if run, task, err := Disable(reg, tasks); err != nil || run || task {
		t.Errorf("nothing to remove: %v %v %v", run, task, err)
	}
	Enable(reg, tasks, agentPath, false)
	tasks.tasks[TaskName] = "x"
	run, task, err := Disable(reg, tasks)
	if err != nil || !run || !task {
		t.Errorf("removed run=%v task=%v err=%v", run, task, err)
	}
	if st, _ := Status(reg, tasks); st.Enabled() || st.Describe() != "does not start automatically" {
		t.Errorf("status after disable = %+v", st)
	}
}
