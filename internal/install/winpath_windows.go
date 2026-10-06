//go:build windows

package install

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// WindowsUserPath is the real per-user PATH: the Path value under
// HKEY_CURRENT_USER\Environment.
//
// It is edited in the registry directly rather than with PowerShell's
// [Environment]::SetEnvironmentVariable because that call quietly replaces
// %USERPROFILE%-style references in the existing PATH with their current
// values and changes the value's type. Here the old text is kept byte for byte
// and the value stays an "expandable string" (REG_EXPAND_SZ).
type WindowsUserPath struct{}

const envKey = `Environment`

// Get implements PathStore.
func (WindowsUserPath) Get() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path") // raw: %VARIABLES% are not expanded
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return v, err
}

// Set implements PathStore.
func (WindowsUserPath) Set(value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, envKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetExpandStringValue("Path", value); err != nil {
		return err
	}
	return broadcastSettingChange()
}

// broadcastSettingChange tells Explorer and other programs that environment
// variables changed, so NEW terminals see the new PATH without a log-off.
func broadcastSettingChange() error {
	const (
		hwndBroadcast   = 0xFFFF
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return err
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	var result uintptr
	r, _, callErr := proc.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)),
		smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
	if r == 0 {
		// The value is saved; programs just won't hear about it until they restart.
		return fmt.Errorf("saved, but couldn't notify running programs (%v): open a new terminal or sign out and in", callErr)
	}
	return nil
}

// ScheduleRemoval deletes the named files in dir - and then dir, if it is empty -
// a moment after this program exits. (Windows won't delete a running .exe, so
// the running program can't remove itself.) Only the listed names are touched
// and the folder is removed with a plain "rmdir", which refuses to delete a
// folder that still has something in it.
func ScheduleRemoval(dir string, names []string) error {
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join(dir, n))
	}
	cmdLine, err := removalCommandLine(dir, paths)
	if err != nil {
		return err
	}
	return startRaw(cmdLine)
}

// startRaw starts cmd.exe with an exact command line (Go's usual argument
// quoting would turn the quotes in it into \", which cmd.exe doesn't understand),
// detached, with no window.
func startRaw(cmdLine string) error {
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       cmdLine,
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the clean-up: %w", err)
	}
	return cmd.Process.Release()
}
