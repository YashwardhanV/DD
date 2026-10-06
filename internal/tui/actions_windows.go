//go:build windows

package tui

import (
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/atotto/clipboard"
	"golang.org/x/sys/windows"
)

// DefaultActions are the real Windows actions.
func DefaultActions() Actions {
	return Actions{Open: shellOpen, ShowInFolder: showInFolder, CopyText: clipboard.WriteAll}
}

// shellOpen opens a file with its default program - exactly what double-clicking
// it in Explorer does. For a folder it opens the folder in Explorer.
func shellOpen(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, dir, windows.SW_SHOWNORMAL)
}

// showInFolder opens Explorer with the file selected.
//
// The command line is built by hand: Go's normal argument quoting would turn
// `/select,"C:\My Folder\a.txt"` into something explorer.exe misreads and it
// would open the wrong folder.
func showInFolder(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	_ = cmd.Run() // explorer.exe exits with status 1 even when it worked, so ignore it
	return nil
}
