package phone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- FindADB ---------------------------------------------------------------

// stubLookup replaces the machine-specific lookups for one test.
func stubLookup(t *testing.T, onPath string, existing []string, localAppData string) {
	t.Helper()
	oldLook, oldExists, oldEnv := lookPath, fileExists, getenv
	t.Cleanup(func() { lookPath, fileExists, getenv = oldLook, oldExists, oldEnv })
	lookPath = func(string) (string, error) {
		if onPath == "" {
			return "", exec.ErrNotFound
		}
		return onPath, nil
	}
	fileExists = func(p string) bool {
		for _, e := range existing {
			if p == e {
				return true
			}
		}
		return false
	}
	getenv = func(k string) string {
		if k == "LOCALAPPDATA" {
			return localAppData
		}
		return ""
	}
}

func TestFindADBSearchOrder(t *testing.T) {
	local := filepath.Join("C:", "Users", "me", "AppData", "Local")
	sdk := filepath.Join(local, "Android", "Sdk", "platform-tools", "adb.exe")
	winget := filepath.Join(local, "Microsoft", "WinGet", "Links", "adb.exe")
	cfg := filepath.Join("D:", "tools", "adb.exe")

	cases := []struct {
		name       string
		configured string
		onPath     string
		existing   []string
		want       string
	}{
		{"config wins over everything", cfg, "/bin/adb", []string{cfg, sdk, winget}, cfg},
		{"PATH next", "", "/bin/adb", []string{sdk, winget}, "/bin/adb"},
		{"Android Studio SDK", "", "", []string{sdk, winget}, sdk},
		{"winget link last", "", "", []string{winget}, winget},
	}
	for _, c := range cases {
		stubLookup(t, c.onPath, c.existing, local)
		got, err := FindADB(c.configured)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestFindADBNotFound(t *testing.T) {
	stubLookup(t, "", nil, `C:\Users\me\AppData\Local`)
	if _, err := FindADB(""); !errors.Is(err, ErrADBNotFound) {
		t.Errorf("err = %v, want ErrADBNotFound", err)
	}
	// A configured path that doesn't exist is an error, not a silent fallback to PATH.
	stubLookup(t, "/bin/adb", nil, "")
	if _, err := FindADB(`D:\nope\adb.exe`); !errors.Is(err, ErrADBNotFound) || !strings.Contains(err.Error(), "adb_path") {
		t.Errorf("bad configured path: %v", err)
	}
	if !strings.Contains(SetupSteps, "Build number") || !strings.Contains(SetupSteps, "winget install --id Google.PlatformTools -e") {
		t.Error("setup steps incomplete")
	}
}

// ---- ExecRunner, using this test binary as a stand-in for adb --------------

// helperCommand makes ExecRunner start THIS test binary (running TestHelperProcess)
// instead of the real adb. It works the same on Windows, Linux and macOS.
func helperCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cs := append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], cs...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	return cmd
}

// TestHelperProcess is not a real test: it is the fake adb. It behaves
// according to the arguments after "--".
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch strings.Join(args, " ") {
	case "devices -l":
		fmt.Print("List of devices attached\nR58 device model:Pixel_7\n")
	case "-s R58 shell lines":
		fmt.Print("one\ntwo\r\nthree\n")
	case "-s R58 shell lines-then-fail":
		fmt.Print("one\ntwo\n")
		fmt.Fprintln(os.Stderr, "find: permission denied")
		os.Exit(1)
	case "-s R58 shell fail":
		fmt.Fprintln(os.Stderr, "error: device unauthorized.\nsecond line")
		os.Exit(1)
	case "-s R58 shell hang":
		fmt.Println("started")
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestExecRunnerOutput(t *testing.T) {
	r := &ExecRunner{Path: "adb", command: helperCommand}
	adb := &ADB{Path: "adb", run: r}
	devs, err := adb.Devices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Serial != "R58" || devs[0].Model != "Pixel 7" || !devs[0].Usable() {
		t.Errorf("devices = %+v, %v", devs, err)
	}
}

func TestExecRunnerOutputExitErrorCarriesStderr(t *testing.T) {
	r := &ExecRunner{Path: "adb", command: helperCommand}
	_, err := r.Output(context.Background(), "-s", "R58", "shell", "fail")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(ee.Stderr, "device unauthorized") {
		t.Fatalf("err = %#v", err)
	}
	if got := ee.Error(); !strings.Contains(got, "status 1") || !strings.Contains(got, "device unauthorized.") || strings.Contains(got, "second line") {
		t.Errorf("message should show only the first stderr line: %q", got)
	}
}

func TestExecRunnerStream(t *testing.T) {
	r := &ExecRunner{Path: "adb", command: helperCommand}

	out, wait, err := r.Stream(context.Background(), "-s", "R58", "shell", "lines")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	buf := make([]byte, 1024)
	n, _ := out.Read(buf)
	for _, l := range strings.Split(strings.TrimSpace(string(buf[:n])), "\n") {
		got = append(got, strings.TrimRight(l, "\r"))
	}
	if err := wait(); err != nil || strings.Join(got, ",") != "one,two,three" {
		t.Errorf("lines=%q err=%v", got, err)
	}

	// Exit status 1 after producing output: wait reports it, the output is still readable.
	out, wait, _ = r.Stream(context.Background(), "-s", "R58", "shell", "lines-then-fail")
	n, _ = out.Read(buf)
	var ee *ExitError
	if err := wait(); !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(ee.Stderr, "permission denied") || !strings.Contains(string(buf[:n]), "one") {
		t.Errorf("wait err = %v, output %q", err, buf[:n])
	}
}

func TestExecRunnerStreamCancellationKillsTheProcess(t *testing.T) {
	r := &ExecRunner{Path: "adb", command: helperCommand}
	ctx, cancel := context.WithCancel(context.Background())
	out, wait, err := r.Stream(ctx, "-s", "R58", "shell", "hang")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	out.Read(buf) // wait until the child is really running
	cancel()

	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process was not killed after cancellation")
	}
}

func TestExecRunnerMissingProgram(t *testing.T) {
	r := &ExecRunner{Path: filepath.Join(t.TempDir(), "no-such-adb")}
	if _, err := r.Output(context.Background(), "devices"); err == nil {
		t.Error("expected an error when adb can't be started")
	}
	if _, _, err := r.Stream(context.Background(), "devices"); err == nil {
		t.Error("expected an error when adb can't be started")
	}
}

func TestLimitedBufferKeepsOnlyTheStart(t *testing.T) {
	var b limitedBuffer
	big := strings.Repeat("e", maxStderr*3)
	if n, err := b.Write([]byte(big)); n != len(big) || err != nil {
		t.Errorf("Write = %d,%v; must claim everything was written", n, err)
	}
	if b.Len() != maxStderr {
		t.Errorf("kept %d bytes, want %d", b.Len(), maxStderr)
	}
}

// ---- StatPath and Pull ------------------------------------------------------------

func TestStatPath(t *testing.T) {
	answers := map[string]string{
		"/sdcard/DCIM":              "directory|4096|1700000000\n",
		"/sdcard/a.jpg":             "regular file|1234|1700000100\r\n", // old adb: CRLF
		"/sdcard/empty":             "regular empty file|0|5\n",
		`'/sdcard/My Photos/b.jpg'`: "regular file|9|7\n",
	}
	var seen []string
	adb := NewADBWithRunner(runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		seen = append(seen, args[len(args)-1])
		for p, out := range answers {
			if strings.Contains(args[len(args)-1], p) || strings.Contains(args[len(args)-1], strings.Trim(p, "'")) && strings.Contains(p, "'") {
				return []byte(out), nil
			}
		}
		return nil, &ExitError{Code: 1} // stat: no such file (and 2>/dev/null hides the message)
	}))
	ctx := context.Background()

	if info, err := adb.StatPath(ctx, "S", "/sdcard/DCIM"); err != nil || !info.Exists || !info.IsDir || info.ModTime != 1700000000 {
		t.Errorf("folder: %+v %v", info, err)
	}
	if info, err := adb.StatPath(ctx, "S", "/sdcard/a.jpg"); err != nil || !info.Exists || info.IsDir || info.Size != 1234 || info.ModTime != 1700000100 {
		t.Errorf("file: %+v %v", info, err)
	}
	if info, _ := adb.StatPath(ctx, "S", "/sdcard/empty"); info.IsDir || !info.Exists || info.Size != 0 {
		t.Errorf("empty file: %+v", info)
	}
	if info, err := adb.StatPath(ctx, "S", "/sdcard/nope"); err != nil || info.Exists {
		t.Errorf("missing: %+v %v", info, err)
	}
	if info, _ := adb.StatPath(ctx, "S", "/sdcard/My Photos/b.jpg"); !info.Exists {
		t.Errorf("a path with a space must be quoted for the phone's shell; sent %q", seen[len(seen)-1])
	}
	// An adb that can't even start is an error, not "doesn't exist".
	broken := NewADBWithRunner(runnerFunc(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("adb not running") }))
	if _, err := broken.StatPath(ctx, "S", "/x"); err == nil {
		t.Error("an adb failure must be reported")
	}
}

func TestPullBuildsTheRightCommand(t *testing.T) {
	var got []string
	adb := NewADBWithRunner(runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) { got = args; return nil, nil }))
	if err := adb.Pull(context.Background(), "S", []string{"/sdcard/a.jpg", "/sdcard/b c.jpg"}, `D:\stage`); err != nil {
		t.Fatal(err)
	}
	want := []string{"-s", "S", "pull", "-a", "/sdcard/a.jpg", "/sdcard/b c.jpg", `D:\stage`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
	got = nil
	if err := adb.Pull(context.Background(), "S", nil, "x"); err != nil || got != nil {
		t.Error("pulling nothing must not run adb")
	}
	failing := NewADBWithRunner(runnerFunc(func(context.Context, ...string) ([]byte, error) {
		return nil, &ExitError{Code: 1, Stderr: "adb: error: remote object doesn't exist"}
	}))
	var ee *ExitError
	if err := failing.Pull(context.Background(), "S", []string{"/x"}, "d"); !errors.As(err, &ee) || !strings.Contains(err.Error(), "adb pull") {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := NewADBWithRunner(runnerFunc(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("killed") }))
	if err := cancelled.Pull(ctx, "S", []string{"/x"}, "d"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// runnerFunc adapts a function to the Runner interface (Output only).
type runnerFunc func(ctx context.Context, args ...string) ([]byte, error)

func (f runnerFunc) Output(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}
func (f runnerFunc) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("not supported by this test runner")
}
