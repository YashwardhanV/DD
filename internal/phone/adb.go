// Package phone indexes Android phones through ADB (Android Debug Bridge),
// bypassing MTP.
//
// Why: when Windows browses a phone over MTP, every folder listing is a
// separate request/response to the phone, one at a time. With ADB the PC sends
// ONE command - "find every file and print its size and date" - the phone runs
// it locally at full speed, and the whole list streams back in one response.
//
// DriveDex only ever reads from the phone (find, stat, pull). It never writes.
package phone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SetupSteps tells the owner how to get a phone ready. Shown whenever adb
// is missing or the phone refuses.
const SetupSteps = `1. On the phone: Settings -> About phone -> tap "Build number" 7 times (turns on Developer options).
2. Settings -> System -> Developer options -> turn on "USB debugging".
3. On the PC install Platform-Tools:  winget install --id Google.PlatformTools -e
   (or download it from https://developer.android.com/tools/releases/platform-tools and unzip it), then open a new terminal.
4. Plug in the phone, unlock it, and tap "Allow" on the "Allow USB debugging?" prompt (tick "Always allow").
5. Check that 'adb devices' shows "<serial>    device".
iPhones are not supported (they have no ADB).`

// ErrADBNotFound means adb.exe could not be located.
var ErrADBNotFound = errors.New("adb (Android Debug Bridge) was not found")

// ExitError is returned when adb (or the command it ran on the phone) exits
// with a non-zero status.
type ExitError struct {
	Code   int
	Stderr string // the first few KB of adb's error output
}

func (e *ExitError) Error() string {
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return fmt.Sprintf("adb exited with status %d: %s", e.Code, firstLine(s))
	}
	return fmt.Sprintf("adb exited with status %d", e.Code)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// Runner runs adb. The real one (ExecRunner) starts adb.exe; tests use a fake,
// so everything built on top can be tested without a phone.
type Runner interface {
	// Output runs adb with args and returns everything it printed on stdout.
	Output(ctx context.Context, args ...string) ([]byte, error)
	// Stream starts adb and returns its stdout as it is produced. The caller
	// must read it to the end (or close it) and then call wait, which returns
	// *ExitError if adb finished with a non-zero status.
	Stream(ctx context.Context, args ...string) (stdout io.ReadCloser, wait func() error, err error)
}

// ADB is a handle for talking to adb.
type ADB struct {
	Path string // for display and diagnostics
	run  Runner
}

// NewADB returns an ADB that runs the adb executable at path.
func NewADB(path string) *ADB { return &ADB{Path: path, run: &ExecRunner{Path: path}} }

// NewADBWithRunner returns an ADB using a custom Runner (tests).
func NewADBWithRunner(r Runner) *ADB { return &ADB{Path: "adb", run: r} }

// ---------------------------------------------------------------------------
// Finding adb
// ---------------------------------------------------------------------------

// Hooks so tests don't depend on what is installed on the machine.
var (
	lookPath   = exec.LookPath
	fileExists = func(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
	getenv     = os.Getenv
)

// FindADB looks for adb.exe in this order:
//  1. the path from the config file (configured; may be empty)
//  2. anywhere on PATH
//  3. %LOCALAPPDATA%\Android\Sdk\platform-tools\adb.exe  (Android Studio)
//  4. %LOCALAPPDATA%\Microsoft\WinGet\Links\adb.exe      (winget install)
//
// If none exists the error wraps ErrADBNotFound.
func FindADB(configured string) (string, error) {
	if configured != "" {
		if fileExists(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("%w: the configured adb_path %q doesn't exist", ErrADBNotFound, configured)
	}
	if p, err := lookPath("adb"); err == nil {
		return p, nil
	}
	if local := getenv("LOCALAPPDATA"); local != "" {
		for _, p := range []string{
			filepath.Join(local, "Android", "Sdk", "platform-tools", "adb.exe"),
			filepath.Join(local, "Microsoft", "WinGet", "Links", "adb.exe"),
		} {
			if fileExists(p) {
				return p, nil
			}
		}
	}
	return "", ErrADBNotFound
}

// ---------------------------------------------------------------------------
// The real runner
// ---------------------------------------------------------------------------

// ExecRunner runs the adb executable.
type ExecRunner struct {
	Path string
	// command builds the process. nil means exec.CommandContext. Tests swap it
	// to run a stand-in program instead of the real adb.
	command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (r *ExecRunner) cmd(ctx context.Context, args []string) *exec.Cmd {
	if r.command != nil {
		return r.command(ctx, r.Path, args...)
	}
	return exec.CommandContext(ctx, r.Path, args...)
}

// maxStderr is how much of adb's error output we keep.
const maxStderr = 8 << 10

// limitedBuffer keeps only the first maxStderr bytes written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxStderr - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil // pretend everything was written so the process never blocks
}

// exitErr converts a process error into *ExitError where appropriate.
func exitErr(err error, stderr string) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode(), Stderr: stderr}
	}
	return err
}

// Output implements Runner.
func (r *ExecRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.cmd(ctx, args)
	var stdout bytes.Buffer
	var stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), ctx.Err()
		}
		return stdout.Bytes(), exitErr(err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Stream implements Runner.
func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	cmd := r.cmd(ctx, args)
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("starting adb: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting adb: %w", err)
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return exitErr(err, stderr.String())
		}
		return nil
	}
	return out, wait, nil
}
