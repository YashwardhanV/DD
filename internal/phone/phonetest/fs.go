package phonetest

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashwardhanv/dd/internal/phone"
)

// File is one file on the fake phone.
type File struct {
	Content string
	MTime   int64
}

// FS is a fake phone: a file system plus an adb that understands the commands
// DriveDex sends (devices, stat, find, pull). Tests fill it with Add and then
// use ADB() wherever a real *phone.ADB is expected.
type FS struct {
	// Devices are the lines `adb devices -l` prints. Defaults to one Pixel 7.
	Devices []string
	// Unreadable files make every pull of that path fail (other files in the
	// same pull still arrive, like real adb).
	Unreadable map[string]bool
	// FailTimes: this many pulls of a path fail before it works.
	FailTimes map[string]int
	// OnPull, if set, runs at the start of every pull (to slow it down or cancel).
	OnPull func(ctx context.Context)

	mu      sync.Mutex
	files   map[string]File
	pulls   [][]string
	running atomic.Int64
	peak    atomic.Int64
}

// NewFS returns an empty fake phone with the default Pixel 7 attached.
func NewFS() *FS {
	return &FS{Devices: []string{Pixel}, Unreadable: map[string]bool{}, FailTimes: map[string]int{}, files: map[string]File{}}
}

// Add puts a file (absolute phone path such as "/sdcard/DCIM/a.jpg") on the phone.
func (f *FS) Add(p, content string, mtime int64) {
	f.mu.Lock()
	f.files[p] = File{content, mtime}
	f.mu.Unlock()
}

// ADB returns an *phone.ADB backed by this fake phone.
func (f *FS) ADB() *phone.ADB {
	adb, _ := New(f.handle)
	return adb
}

// PullCalls returns the remote paths of every `adb pull` so far.
func (f *FS) PullCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.pulls))
	for i, c := range f.pulls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// PullCount is the number of `adb pull` calls so far.
func (f *FS) PullCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pulls) }

// PeakPulls is the highest number of pulls that were running at the same time.
func (f *FS) PeakPulls() int { return int(f.peak.Load()) }

func (f *FS) handle(ctx context.Context, args []string) (string, error) {
	if len(args) > 0 && args[0] == "devices" {
		return DevicesOutput(f.Devices...), nil
	}
	if len(args) == 1 && args[0] == "version" {
		return "Android Debug Bridge version 1.0.41\nVersion 35.0.2-12147458\nInstalled as C:\\platform-tools\\adb.exe\n", nil
	}
	if len(args) < 3 || args[0] != "-s" {
		return "", fmt.Errorf("phonetest: unexpected adb call %q", args)
	}
	switch args[2] {
	case "pull":
		return f.pull(ctx, args[3:]) // -a remote... local
	case "shell":
		cmd := args[3]
		switch {
		case strings.HasPrefix(cmd, "stat -c"):
			return f.stat(cmd), nil
		case strings.HasPrefix(cmd, "find "):
			return f.find(cmd), nil
		}
	}
	return "", fmt.Errorf("phonetest: unexpected adb call %q", args)
}

// unquote extracts a path from `'quoted path'` or a bare word.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") {
		s = s[1:]
		return strings.ReplaceAll(s[:strings.Index(s, "'")], `'\''`, "'")
	}
	return strings.Fields(s)[0]
}

func (f *FS) stat(cmd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := unquote(strings.TrimPrefix(cmd, "stat -c '%F|%s|%Y' "))
	if file, ok := f.files[target]; ok {
		return fmt.Sprintf("regular file|%d|%d\n", len(file.Content), file.MTime)
	}
	for p := range f.files {
		if strings.HasPrefix(p, strings.TrimRight(target, "/")+"/") {
			return "directory|4096|1\n"
		}
	}
	return ""
}

func (f *FS) find(cmd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := strings.TrimRight(unquote(strings.TrimPrefix(cmd, "find ")), "/") + "/"
	var paths []string
	for p := range f.files {
		if strings.HasPrefix(p, root) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var sb strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&sb, "%d|%d|%s\n", len(f.files[p].Content), f.files[p].MTime, p)
	}
	return sb.String()
}

func (f *FS) pull(ctx context.Context, args []string) (string, error) {
	if len(args) < 3 || args[0] != "-a" {
		return "", fmt.Errorf("phonetest: unexpected pull args %q", args)
	}
	remotes, local := args[1:len(args)-1], args[len(args)-1]

	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	f.mu.Lock()
	f.pulls = append(f.pulls, append([]string(nil), remotes...))
	hook := f.OnPull
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}

	failed := false
	for _, r := range remotes {
		f.mu.Lock()
		file, ok := f.files[r]
		bad := f.Unreadable[r]
		if f.FailTimes[r] > 0 {
			f.FailTimes[r]--
			bad = true
		}
		f.mu.Unlock()
		if !ok || bad {
			failed = true
			continue
		}
		dst := filepath.Join(local, path.Base(r))
		if st, err := os.Stat(local); len(remotes) == 1 && (err != nil || !st.IsDir()) {
			dst = local // like adb: one file pulled to a path that isn't a folder is saved under that name
		}
		if err := os.WriteFile(dst, []byte(file.Content), 0o644); err != nil {
			return "", err
		}
		t := time.Unix(file.MTime, 0)
		os.Chtimes(dst, t, t) // `adb pull -a` keeps the phone's modification time
	}
	if failed {
		return "", &phone.ExitError{Code: 1, Stderr: "adb: error: failed to copy some files"}
	}
	return "", nil
}
