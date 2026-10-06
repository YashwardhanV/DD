//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

const exeSuffix = ".exe"

// AcquireInstance makes sure only one agent runs per user session, using a
// named Windows mutex. already is true when another agent holds it; release
// frees it.
func AcquireInstance() (release func(), already bool, err error) {
	name, err := windows.UTF16PtrFromString(`Local\DriveDexAgent`)
	if err != nil {
		return nil, false, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("creating the single-instance mutex: %w", err)
	}
	return func() { windows.CloseHandle(h) }, false, nil
}

// stillActive is the exit code Windows reports for a process that hasn't finished.
const stillActive = 259

// ProcessRunning reports whether a process with this ID is alive.
func ProcessRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// KillProcess ends a process.
func KillProcess(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 0); err != nil {
		return fmt.Errorf("stopping process %d: %w", pid, err)
	}
	return nil
}

// StartDetached launches a program that keeps running after this one exits
// and has no console window.
func StartDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", exe, err)
	}
	return cmd.Process.Release()
}

// RunHidden runs a program to completion without showing a console window and
// returns its combined output (used by the agent to run `drivedex index`).
func RunHidden(cmd *exec.Cmd) ([]byte, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.CombinedOutput()
}
