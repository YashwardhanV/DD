//go:build windows

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	ansiSupported = false

	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleOutputCP         = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableVirtualTerminalProcessing = 0x0004
	cpUTF8                          = 65001
)

// SetupConsole makes the Windows console show colors and Unicode file names
// properly, and returns a function that puts the console back the way it was.
//
//   - ENABLE_VIRTUAL_TERMINAL_PROCESSING lets the console understand ANSI
//     color codes (older consoles print them as garbage otherwise).
//   - Code page 65001 is UTF-8; without it names like "写真.png" show as "?".
func SetupConsole() (restore func()) {
	restore = func() {}

	h := os.Stdout.Fd()
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r != 0 {
		if mode&enableVirtualTerminalProcessing != 0 {
			ansiSupported = true
		} else if r, _, _ := procSetConsoleMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing)); r != 0 {
			ansiSupported = true
			prev := restore
			restore = func() { procSetConsoleMode.Call(h, uintptr(mode)); prev() }
		}
		// stdout is a real console: also switch its code page to UTF-8.
		if cp, _, _ := procGetConsoleOutputCP.Call(); cp != 0 && cp != cpUTF8 {
			if r, _, _ := procSetConsoleOutputCP.Call(cpUTF8); r != 0 {
				prev := restore
				restore = func() { procSetConsoleOutputCP.Call(cp); prev() }
			}
		}
	}
	return restore
}

// consoleScreenBufferInfo mirrors the Win32 CONSOLE_SCREEN_BUFFER_INFO struct.
type consoleScreenBufferInfo struct {
	SizeX, SizeY       int16
	CursorX, CursorY   int16
	Attributes         uint16
	Left, Top          int16
	Right, Bottom      int16
	MaxSizeX, MaxSizeY int16
}

// termWidth returns the width of the console window in columns.
func termWidth() int {
	var info consoleScreenBufferInfo
	for _, f := range []*os.File{os.Stderr, os.Stdout} {
		if r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r != 0 {
			if w := int(info.Right-info.Left) + 1; w > 0 {
				return w
			}
		}
	}
	return envWidth(os.Getenv("COLUMNS"))
}
