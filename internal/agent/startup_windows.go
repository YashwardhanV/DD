//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// WindowsRegistry implements Registry with the real per-user Run key.
type WindowsRegistry struct{}

func (WindowsRegistry) open(access uint32) (registry.Key, error) {
	return registry.OpenKey(registry.CURRENT_USER, RunKeyPath, access)
}

// Get implements Registry.
func (r WindowsRegistry) Get(name string) (string, bool, error) {
	k, err := r.open(registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	return v, err == nil, err
}

// Set implements Registry.
func (r WindowsRegistry) Set(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, RunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

// Delete implements Registry.
func (r WindowsRegistry) Delete(name string) (bool, error) {
	k, err := r.open(registry.SET_VALUE | registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer k.Close()
	if _, _, err := k.GetStringValue(name); errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err := k.DeleteValue(name); err != nil {
		return false, err
	}
	return true, nil
}

// WindowsTasks implements Tasks with schtasks.exe.
type WindowsTasks struct{}

func schtasks(args ...string) ([]byte, error) {
	cmd := exec.Command("schtasks.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.CombinedOutput()
}

// Create implements Tasks. The command already contains its own quotes; Go's
// argument quoting turns them into the \" form schtasks expects:
//
//	schtasks /Create /TN "DriveDex Agent" /TR "\"C:\...\drivedex-agent.exe\"" /SC ONLOGON /RL HIGHEST /F
func (WindowsTasks) Create(name, command string) error {
	out, err := schtasks("/Create", "/TN", name, "/TR", command, "/SC", "ONLOGON", "/RL", "HIGHEST", "/F")
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(strings.ToLower(text), "access is denied") {
			return ErrNeedAdminForTask
		}
		return fmt.Errorf("schtasks /Create: %w (%s)", err, firstLineOf(text))
	}
	return nil
}

// Exists implements Tasks.
func (WindowsTasks) Exists(name string) (bool, error) {
	_, err := schtasks("/Query", "/TN", name)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // a non-zero exit means "no such task"
		}
		return false, err
	}
	return true, nil
}

// Delete implements Tasks.
func (t WindowsTasks) Delete(name string) (bool, error) {
	exists, err := t.Exists(name)
	if err != nil || !exists {
		return false, err
	}
	out, err := schtasks("/Delete", "/TN", name, "/F")
	if err != nil {
		return false, fmt.Errorf("schtasks /Delete: %w (%s)", err, firstLineOf(string(out)))
	}
	return true, nil
}
