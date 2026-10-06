package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/index"
)

// agentEnv fakes the operating-system side of the agent commands.
type agentEnv struct {
	t       *testing.T
	dir     string
	mu      sync.Mutex
	alive   map[int]bool // fake process table
	started []string
	killed  []int
	reg     map[string]string
	tasks   map[string]string
	exeErr  error
	needAdm bool
	noSync  bool // true: "start" launches nothing that ever writes the PID file
}

func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	e := &agentEnv{t: t, dir: t.TempDir(), alive: map[int]bool{}, reg: map[string]string{}, tasks: map[string]string{}}
	oldDir, oldStart, oldKill, oldRun, oldExe, oldReg, oldTasks, oldWait := agentDataDir, agentStart, agentKill, processRunning, agentExe, newStartupRegistry, newStartupTasks, agentWait
	t.Cleanup(func() {
		agentDataDir, agentStart, agentKill, processRunning, agentExe, newStartupRegistry, newStartupTasks, agentWait = oldDir, oldStart, oldKill, oldRun, oldExe, oldReg, oldTasks, oldWait
	})
	agentWait = 400 * time.Millisecond
	agentDataDir = func() (string, error) { return e.dir, nil }
	agentExe = func() (string, error) {
		if e.exeErr != nil {
			return "", e.exeErr
		}
		return `C:\DriveDex\drivedex-agent.exe`, nil
	}
	processRunning = func(pid int) bool { e.mu.Lock(); defer e.mu.Unlock(); return e.alive[pid] }
	agentStart = func(exe string, args ...string) error {
		e.mu.Lock()
		e.started = append(e.started, exe)
		e.mu.Unlock()
		if !e.noSync {
			e.runAgent(4242) // the "agent" comes up and writes its PID file
		}
		return nil
	}
	agentKill = func(pid int) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.killed = append(e.killed, pid)
		e.alive[pid] = false
		return nil
	}
	newStartupRegistry = func() agent.Registry { return &memReg{e} }
	newStartupTasks = func() agent.Tasks { return &memTasks{e} }
	return e
}

func (e *agentEnv) runAgent(pid int) {
	e.mu.Lock()
	e.alive[pid] = true
	e.mu.Unlock()
	os.WriteFile(agent.PIDPath(e.dir), []byte(strconv.Itoa(pid)+"\n"+time.Now().Add(-90*time.Second).Format(time.RFC3339)+"\n"), 0o644)
}

type memReg struct{ e *agentEnv }

func (r *memReg) Get(n string) (string, bool, error) { v, ok := r.e.reg[n]; return v, ok, nil }
func (r *memReg) Set(n, v string) error              { r.e.reg[n] = v; return nil }
func (r *memReg) Delete(n string) (bool, error) {
	_, ok := r.e.reg[n]
	delete(r.e.reg, n)
	return ok, nil
}

type memTasks struct{ e *agentEnv }

func (t *memTasks) Create(n, c string) error {
	if t.e.needAdm {
		return agent.ErrNeedAdminForTask
	}
	t.e.tasks[n] = c
	return nil
}
func (t *memTasks) Exists(n string) (bool, error) { _, ok := t.e.tasks[n]; return ok, nil }
func (t *memTasks) Delete(n string) (bool, error) {
	_, ok := t.e.tasks[n]
	delete(t.e.tasks, n)
	return ok, nil
}

func TestAgentStartStopStatus(t *testing.T) {
	e := newAgentEnv(t)

	out, _ := run(t, "agent", "status")
	if !strings.Contains(out, "not running") || !strings.Contains(out, "drivedex agent start") || !strings.Contains(out, "does not start automatically") {
		t.Errorf("status before start:\n%s", out)
	}

	out, err := run(t, "agent", "start")
	if err != nil || !strings.Contains(out, "Agent started (PID 4242)") || !strings.Contains(out, "agent.log") || len(e.started) != 1 {
		t.Fatalf("start: %v\n%s", err, out)
	}
	// Starting again does nothing.
	out, _ = run(t, "agent", "start")
	if !strings.Contains(out, "already running (PID 4242)") || len(e.started) != 1 {
		t.Errorf("second start:\n%s", out)
	}

	// Status shows PID, uptime and the log tail.
	l, _ := agent.NewLogger(agent.LogPath(e.dir))
	for i := 1; i <= 12; i++ {
		l.Infof("event %d", i)
	}
	l.Close()
	out, _ = run(t, "agent", "status")
	for _, want := range []string{"running", "PID 4242", "up 1m", "Last log lines", "event 12", "event 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "event 2\n") || strings.Contains(out, "event 1\n") {
		t.Errorf("only the last 10 log lines belong in the status:\n%s", out)
	}

	out, err = run(t, "agent", "stop")
	if err != nil || !strings.Contains(out, "Agent stopped (was PID 4242)") || len(e.killed) != 1 || e.killed[0] != 4242 {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(agent.PIDPath(e.dir)); statErr == nil {
		t.Error("stop must remove the PID file")
	}
	out, _ = run(t, "agent", "stop")
	if !strings.Contains(out, "The agent isn't running.") || len(e.killed) != 1 {
		t.Errorf("stop when stopped:\n%s", out)
	}
}

func TestAgentStopCleansUpACrashedAgentsPIDFile(t *testing.T) {
	e := newAgentEnv(t)
	os.WriteFile(agent.PIDPath(e.dir), []byte("9999\n"), 0o644) // process 9999 isn't alive
	out, _ := run(t, "agent", "stop")
	if !strings.Contains(out, "isn't running") || len(e.killed) != 0 {
		t.Errorf("out=%s killed=%v", out, e.killed)
	}
	if _, err := os.Stat(agent.PIDPath(e.dir)); err == nil {
		t.Error("the stale PID file should be removed")
	}
	out, _ = run(t, "agent", "status")
	if !strings.Contains(out, "not running") {
		t.Errorf("status with a stale PID file:\n%s", out)
	}
}

func TestAgentStartProblems(t *testing.T) {
	e := newAgentEnv(t)
	e.exeErr = errors.New("not there")
	_, err := run(t, "agent", "start")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "agent program wasn't found") || !strings.Contains(ue.Hint, "build.ps1") {
		t.Errorf("missing exe: %v", err)
	}

	e.exeErr = nil
	e.noSync = true // launches, but never reports in
	_, err = run(t, "agent", "start")
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "didn't report in") || !strings.Contains(ue.Hint, "agent.log") {
		t.Errorf("silent agent: %v", err)
	}

	agentStart = func(string, ...string) error { return errors.New("access denied") }
	if _, err = run(t, "agent", "start"); !errors.As(err, &ue) || !strings.Contains(ue.Msg, "couldn't start") {
		t.Errorf("launch failure: %v", err)
	}
}

func TestStartupEnableDisableStatus(t *testing.T) {
	e := newAgentEnv(t)

	// Declining changes nothing; the question names exactly what will be written.
	var out strings.Builder
	if err := runStartupEnable(&out, strings.NewReader("n\n"), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`) || !strings.Contains(out.String(), "Continue? [y/N]") || !strings.Contains(out.String(), "Cancelled") || len(e.reg) != 0 {
		t.Errorf("decline:\n%s", out.String())
	}
	out.Reset()
	if err := runStartupEnable(&out, strings.NewReader("y\n"), false, false); err != nil {
		t.Fatal(err)
	}
	if e.reg[agent.RunValueName] != `"C:\DriveDex\drivedex-agent.exe"` || !strings.Contains(out.String(), "next log-in") {
		t.Errorf("enable: reg=%v\n%s", e.reg, out.String())
	}

	res, _ := run(t, "startup", "status")
	if !strings.Contains(res, "starts at log-in") {
		t.Errorf("status:\n%s", res)
	}
	res, _ = run(t, "agent", "status")
	if !strings.Contains(res, "Start-up: starts at log-in") {
		t.Errorf("agent status mentions start-up:\n%s", res)
	}

	res, err := run(t, "startup", "disable")
	if err != nil || !strings.Contains(res, "Removed the Run entry") || len(e.reg) != 0 {
		t.Errorf("disable: %v reg=%v\n%s", err, e.reg, res)
	}
	res, _ = run(t, "startup", "disable")
	if !strings.Contains(res, "Nothing to remove") {
		t.Errorf("disable twice:\n%s", res)
	}
}

func TestStartupElevated(t *testing.T) {
	e := newAgentEnv(t)
	out, err := run(t, "startup", "enable", "--elevated", "--yes")
	if err != nil || !strings.Contains(out, "administrator rights") || e.tasks[agent.TaskName] == "" {
		t.Fatalf("%v\n%s", err, out)
	}
	// The question explains the trade-off before asking.
	var q strings.Builder
	runStartupEnable(&q, strings.NewReader("n\n"), true, false)
	if !strings.Contains(q.String(), "ADMINISTRATOR rights") || !strings.Contains(q.String(), "NTFS turbo mode") || !strings.Contains(q.String(), "Administrator terminal") {
		t.Errorf("the elevated question must explain the trade-off:\n%s", q.String())
	}
	out, _ = run(t, "startup", "disable")
	if !strings.Contains(out, "scheduled task") || len(e.tasks) != 0 {
		t.Errorf("disable elevated:\n%s", out)
	}

	e.needAdm = true
	_, err = run(t, "startup", "enable", "--elevated", "--yes")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "needs an Administrator terminal") || !strings.Contains(ue.Hint, "without --elevated") {
		t.Errorf("not admin: %v", err)
	}
}

func TestStartupWithoutTheAgentProgram(t *testing.T) {
	e := newAgentEnv(t)
	e.exeErr = errors.New("missing")
	if _, err := run(t, "startup", "enable", "--yes"); err == nil || !strings.Contains(err.Error(), "agent program wasn't found") {
		t.Errorf("err = %v", err)
	}
}

func TestStartupUnsupportedPlatform(t *testing.T) {
	newAgentEnv(t)
	newStartupRegistry = func() agent.Registry { return agent.WindowsRegistry{} }
	newStartupTasks = func() agent.Tasks { return agent.WindowsTasks{} }
	if os.PathSeparator == '\\' {
		t.Skip("on Windows the real implementations are used")
	}
	for _, args := range [][]string{{"startup", "enable", "--yes"}, {"startup", "disable"}, {"startup", "status"}} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "only work on Windows") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// ---- the shared indexing lock --------------------------------------------------------------

func TestIndexRefusesWhileAnotherProcessHoldsTheLock(t *testing.T) {
	tree, _ := withFixture(t)
	store, _ := openStore()
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The background agent is already indexing this folder.
	id := indexIDOfFolder(tree)
	release, err := agent.AcquireLock(store.LockPath(id))
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "index", tree)
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "already being indexed by another DriveDex process") || !errors.Is(err, agent.ErrLocked) {
		t.Fatalf("got %v", err)
	}
	if store.Exists(id) {
		t.Error("nothing may be written while someone else holds the lock")
	}

	release()
	out, err := run(t, "index", tree)
	if err != nil || !strings.Contains(out, "Indexed ") {
		t.Fatalf("after release: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(store.LockPath(id)); statErr == nil {
		t.Error("the lock file must be removed after indexing")
	}
}

func TestLockIsReleasedEvenWhenIndexingFails(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	store, _ := openStore()
	bad := filepath.Join(t.TempDir(), "missing")
	run(t, "index", bad) // fails before locking (the path doesn't exist)
	entries, _ := os.ReadDir(store.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Errorf("leftover lock %s", e.Name())
		}
	}
}

// indexIDOfFolder is the SourceID `drivedex index <folder>` will use.
func indexIDOfFolder(path string) string {
	abs, _ := filepath.Abs(path)
	return index.FolderSourceID(abs)
}
