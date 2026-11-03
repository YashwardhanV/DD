package benchcopy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Runner starts a program and waits for it. exitCode is its exit status; err
// is set only when the program couldn't be run at all. Tests use a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (exitCode int, output string, err error)
}

// ExecRunner runs real programs.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out), nil
	}
	if err != nil {
		return -1, string(out), err
	}
	return 0, string(out), nil
}

// lookPath is exec.LookPath (a variable for tests).
var lookPath = exec.LookPath

// goos is runtime.GOOS (a variable so tests can pretend to be Windows).
var goos = runtime.GOOS

// ---- robocopy -----------------------------------------------------------------------------

// Robocopy runs Windows' robocopy. Threads 0 = single thread (no /MT).
type Robocopy struct {
	Run     Runner
	Threads int
}

// Name implements Engine.
func (r Robocopy) Name() string {
	if r.Threads > 0 {
		return "robocopy-mt"
	}
	return "robocopy-1"
}

// Available implements Engine.
func (r Robocopy) Available() error {
	if goos != "windows" {
		return errors.New("robocopy only exists on Windows")
	}
	if _, err := lookPath("robocopy"); err != nil {
		return errors.New("robocopy.exe was not found")
	}
	return nil
}

// RobocopyArgs is the command line: copy src INTO dst\<name of src>, like
// drivedex and Explorer do, with no retries and no per-file output.
func RobocopyArgs(src, dst string, threads int) []string {
	args := []string{src, filepath.Join(dst, folderName(src)), "/E", "/R:0", "/W:0", "/NFL", "/NDL", "/NJH", "/NJS", "/NP"}
	if threads > 0 {
		args = append(args, fmt.Sprintf("/MT:%d", threads))
	}
	return args
}

// RobocopyOK maps robocopy's exit code: 0-7 mean success (bits for "files
// copied", "extra files", "mismatches"), 8 and up mean at least one failure.
func RobocopyOK(code int) bool { return code >= 0 && code <= 7 }

// Copy implements Engine.
func (r Robocopy) Copy(ctx context.Context, src, dst string) error {
	code, out, err := r.Run.Run(ctx, "robocopy", RobocopyArgs(src, dst, r.Threads)...)
	if err != nil {
		return err
	}
	if !RobocopyOK(code) {
		return fmt.Errorf("robocopy exit code %d (8 or more = some files failed): %s", code, lastLine(out))
	}
	return nil
}

// ---- drivedex itself ----------------------------------------------------------------------

// DriveDex runs this program's own copy command, as a separate process (so
// every engine pays the same start-up cost and nothing is shared in memory).
type DriveDex struct {
	Run   Runner
	Exe   string // path of drivedex.exe
	V1    bool   // add --v1: every v2 feature off
	Extra []string
}

// Name implements Engine.
func (d DriveDex) Name() string {
	if d.V1 {
		return "drivedex-v1"
	}
	return "drivedex"
}

// Available implements Engine.
func (d DriveDex) Available() error {
	if d.Exe == "" {
		return errors.New("couldn't find drivedex.exe itself")
	}
	return nil
}

// Args is the command line used.
func (d DriveDex) Args(src, dst string) []string {
	args := []string{"copy", src, dst, "--no-color"}
	if d.V1 {
		args = append(args, "--v1")
	}
	return append(args, d.Extra...)
}

// Copy implements Engine.
func (d DriveDex) Copy(ctx context.Context, src, dst string) error {
	code, out, err := d.Run.Run(ctx, d.Exe, d.Args(src, dst)...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("drivedex copy exit code %d: %s", code, lastLine(out))
	}
	return nil
}

// ---- adb pull (phone sources) ----------------------------------------------------------------

// ADBPull copies a phone folder with one plain 'adb pull -a' of the folder.
type ADBPull struct {
	Run    Runner
	ADB    string // path of adb.exe
	Serial string
}

// Name implements Engine.
func (a ADBPull) Name() string { return "adb-pull" }

// Available implements Engine.
func (a ADBPull) Available() error {
	if a.ADB == "" {
		return errors.New("adb was not found")
	}
	return nil
}

// Copy implements Engine. src is the absolute folder on the phone.
func (a ADBPull) Copy(ctx context.Context, src, dst string) error {
	code, out, err := a.Run.Run(ctx, a.ADB, "-s", a.Serial, "pull", "-a", src, dst)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("adb pull exit code %d: %s", code, lastLine(out))
	}
	return nil
}

// ---- Explorer (experimental) -----------------------------------------------------------------

// Explorer copies with the Windows Shell's own copy engine - the one File
// Explorer uses - through Shell.Application.CopyHere, started from PowerShell
// (no extra Go dependency). CopyHere returns at once and copies in the
// background, so the script itself waits until the destination holds exactly
// the expected files and bytes and has stayed unchanged for 2 s, and reports
// the time at which it first matched.
type Explorer struct {
	Run      Runner
	Expected Size
	// Timeout returns how long to wait at most (2x the slowest other engine
	// so far; the CLI supplies it).
	Timeout func() time.Duration
	last    *time.Duration
}

// NewExplorer returns an Explorer engine.
func NewExplorer(run Runner, expected Size, timeout func() time.Duration) *Explorer {
	return &Explorer{Run: run, Expected: expected, Timeout: timeout, last: new(time.Duration)}
}

// Name implements Engine.
func (e *Explorer) Name() string { return "explorer" }

// Available implements Engine.
func (e *Explorer) Available() error {
	if goos != "windows" {
		return errors.New("the Explorer engine only exists on Windows")
	}
	if e.Expected == (Size{}) {
		return errors.New("the size of the source is unknown, so the end of an Explorer copy can't be detected")
	}
	if _, err := lookPath("powershell"); err != nil {
		return errors.New("powershell.exe was not found")
	}
	return nil
}

// psQuote quotes a string for PowerShell (single quotes, ” inside).
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ExplorerScript is the PowerShell script for one copy. Flags 1556 =
// 4 (no progress window) | 16 (yes to all) | 512 (no confirmation for new
// folders) | 1024 (no error window).
func ExplorerScript(src, dst string, want Size, timeout time.Duration) string {
	return strings.Join([]string{
		`$ErrorActionPreference = 'Stop'`,
		`$sh = New-Object -ComObject Shell.Application`,
		`$target = $sh.NameSpace(` + psQuote(dst) + `)`,
		`$sw = [Diagnostics.Stopwatch]::StartNew()`,
		`$target.CopyHere(` + psQuote(src) + `, 1556)`,
		`$doneAt = -1`,
		fmt.Sprintf(`while ($sw.ElapsedMilliseconds -lt %d) {`, timeout.Milliseconds()),
		`  $f = Get-ChildItem -LiteralPath ` + psQuote(dst) + ` -Recurse -File -Force -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum`,
		fmt.Sprintf(`  if ($f.Count -eq %d -and [int64]$f.Sum -eq %d) {`, want.Files, want.Bytes),
		`    if ($doneAt -lt 0) { $doneAt = $sw.ElapsedMilliseconds }`,
		`    if ($sw.ElapsedMilliseconds - $doneAt -ge 2000) { "DONE $doneAt"; exit 0 }`,
		`  } else { $doneAt = -1 }`,
		`  Start-Sleep -Milliseconds 100`,
		`}`,
		`"TIMEOUT"; exit 3`,
	}, "\n")
}

var doneRe = regexp.MustCompile(`DONE (\d+)`)

// Copy implements Engine.
func (e *Explorer) Copy(ctx context.Context, src, dst string) error {
	timeout := 30 * time.Minute
	if e.Timeout != nil {
		if t := e.Timeout(); t > 0 {
			timeout = t
		}
	}
	*e.last = 0
	code, out, err := e.Run.Run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ExplorerScript(src, dst, e.Expected, timeout))
	if err != nil {
		return err
	}
	m := doneRe.FindStringSubmatch(out)
	if code != 0 || m == nil {
		return fmt.Errorf("the Explorer copy didn't finish within %s (it can't always be measured reliably - time it by stopwatch instead): %s", timeout.Round(time.Second), lastLine(out))
	}
	ms, _ := strconv.ParseInt(m[1], 10, 64)
	*e.last = time.Duration(ms) * time.Millisecond
	return nil
}

// LastElapsed implements SelfTimed.
func (e *Explorer) LastElapsed() time.Duration { return *e.last }

// ---- helpers ---------------------------------------------------------------------------

// folderName is the name a copied folder gets inside the destination.
func folderName(src string) string {
	b := filepath.Base(filepath.Clean(src))
	if b == "." || b == string(filepath.Separator) || strings.HasSuffix(b, ":") || b == "" {
		return "copy"
	}
	return b
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
