//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

const exeSuffix = ""

// AcquireInstance (non-Windows, development only): the PID file is the guard.
func AcquireInstance() (release func(), already bool, err error) {
	dir, err := DataDir()
	if err != nil {
		return nil, false, err
	}
	if _, running := Running(dir); running {
		return nil, true, nil
	}
	return func() {}, false, nil
}

// ProcessRunning reports whether a process with this ID is alive.
func ProcessRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// KillProcess ends a process.
func KillProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// StartDetached launches a program that keeps running after this one exits.
func StartDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RunHidden runs a program to completion and returns its combined output.
func RunHidden(cmd *exec.Cmd) ([]byte, error) { return cmd.CombinedOutput() }
