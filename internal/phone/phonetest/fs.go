package phonetest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
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
	// NoCmin makes the phone's find reject -cmin (like an old busybox).
	NoCmin bool
	// NoTar makes the phone have no working tar (the probe fails).
	NoTar bool
	// Features is what 'adb features' prints (e.g. "sendrecv_v2,sendrecv_v2_lz4").
	Features string
	// TarExtra, if set, can add entries to every tar stream after the
	// requested files (malicious or buggy phone tests).
	TarExtra func(tw *tar.Writer)
	// TarCut cuts the first TarCutTimes tar streams after TarCut bytes, like
	// an unplugged cable.
	TarCut, TarCutTimes int
	// OnStreamRead is called while a tar stream is read, with the bytes read so far.
	OnStreamRead func(ctx context.Context, n int64)
	// Now is the phone's clock in unix seconds (default: the real clock).
	Now func() int64

	mu      sync.Mutex
	files   map[string]File
	ctimes  map[string]int64 // status-change time; default = MTime
	shells  []string         // every shell command run
	tars    [][]string       // the files requested by every tar stream
	algos   []string         // the -z algorithm of every pull ("" = none)
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

// SetCTime sets a file's status-change time (what find -cmin looks at). A
// file added with Add has ctime = mtime until this is called.
func (f *FS) SetCTime(p string, ctime int64) {
	f.mu.Lock()
	if f.ctimes == nil {
		f.ctimes = map[string]int64{}
	}
	f.ctimes[p] = ctime
	f.mu.Unlock()
}

// Remove deletes a file from the phone.
func (f *FS) Remove(p string) {
	f.mu.Lock()
	delete(f.files, p)
	f.mu.Unlock()
}

// ShellCommands returns every 'adb shell' command run so far.
func (f *FS) ShellCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.shells...)
}

func (f *FS) now() int64 {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().Unix()
}

// ADB returns an *phone.ADB backed by this fake phone.
func (f *FS) ADB() *phone.ADB {
	fake := &Fake{Handler: f.handle}
	fake.Wrap = func(args []string, r io.Reader) io.Reader {
		if f.OnStreamRead == nil || len(args) < 3 || args[2] != "exec-out" {
			return r
		}
		return &watchReader{r: r, fn: f.OnStreamRead}
	}
	return phone.NewADBWithRunner(fake)
}

// watchReader reports progress while a stream is read.
type watchReader struct {
	r  io.Reader
	n  int64
	fn func(ctx context.Context, n int64)
}

func (w *watchReader) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096] // small reads, so a test can stop a stream mid-file
	}
	n, err := w.r.Read(p)
	w.n += int64(n)
	w.fn(context.Background(), w.n)
	return n, err
}

// TarCalls returns the requested files of every tar stream so far.
func (f *FS) TarCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.tars))
	for i, c := range f.tars {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// PullAlgos returns the -z algorithm of every pull ("" = plain pull).
func (f *FS) PullAlgos() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.algos...)
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
	if len(args) == 1 && args[0] == "help" {
		return "global options:\n ...\nfile transfer:\n pull [-a] [-z ALGORITHM] [-Z] REMOTE... LOCAL\n", nil
	}
	if len(args) == 1 && args[0] == "version" {
		return "Android Debug Bridge version 1.0.41\nVersion 35.0.2-12147458\nInstalled as C:\\platform-tools\\adb.exe\n", nil
	}
	if len(args) < 3 || args[0] != "-s" {
		return "", fmt.Errorf("phonetest: unexpected adb call %q", args)
	}
	switch args[2] {
	case "pull":
		return f.pull(ctx, args[3:]) // -a [-z algo] remote... local
	case "features":
		return f.Features + "\n", nil
	case "exec-out":
		return f.execOut(args[3])
	case "shell":
		cmd := args[3]
		f.mu.Lock()
		f.shells = append(f.shells, cmd)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "stat -c"):
			return f.stat(cmd), nil
		case strings.HasPrefix(cmd, "find / -maxdepth 0 -cmin"):
			if f.NoCmin {
				return "find: Unknown option '-cmin'\nDDX_RC=1\n", nil
			}
			return "DDX_RC=0\n", nil
		case strings.HasPrefix(cmd, "find ") && strings.Contains(cmd, " -cmin -"):
			if f.NoCmin {
				return "", &phone.ExitError{Code: 1}
			}
			return f.findChanged(cmd), nil
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

// findChanged answers DeltaCommand: files under the root whose ctime is
// within the last N minutes.
func (f *FS) findChanged(cmd string) string {
	root := strings.TrimRight(unquote(strings.TrimPrefix(cmd, "find ")), "/") + "/"
	i := strings.Index(cmd, " -cmin -")
	var minutes int64
	fmt.Sscanf(cmd[i+len(" -cmin -"):], "%d", &minutes)
	since := f.now() - minutes*60
	f.mu.Lock()
	defer f.mu.Unlock()
	var paths []string
	for p, file := range f.files {
		ct, ok := f.ctimes[p]
		if !ok {
			ct = file.MTime
		}
		if strings.HasPrefix(p, root) && ct > since {
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
	args = args[1:]
	algo := ""
	if len(args) > 2 && args[0] == "-z" {
		algo, args = args[1], args[2:]
	}
	f.mu.Lock()
	f.algos = append(f.algos, algo)
	f.mu.Unlock()
	remotes, local := args[:len(args)-1], args[len(args)-1]

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

// execOut answers 'adb exec-out': the tar probe and tar streams.
func (f *FS) execOut(cmd string) (string, error) {
	if cmd == phone.TarProbeCommand {
		if f.NoTar {
			return "tar: unknown option -C\n", nil
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "hosts", Mode: 0o644, Size: 9, Typeflag: tar.TypeReg})
		tw.Write([]byte("127.0.0.1"))
		tw.Close()
		return buf.String(), nil
	}
	root, rels, ok := parseTarCommand(cmd)
	if !ok || f.NoTar {
		return "", &phone.ExitError{Code: 127, Stderr: "tar: not found"}
	}
	f.mu.Lock()
	f.tars = append(f.tars, append([]string(nil), rels...))
	cut := 0
	if f.TarCutTimes > 0 {
		f.TarCutTimes--
		cut = f.TarCut
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	missing := false
	for _, rel := range rels {
		full := path.Join(root, rel)
		file, ok := f.files[full]
		if !ok || f.Unreadable[full] {
			missing = true // real tar: "No such file", carries on, exits 1
			continue
		}
		tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(file.Content)), ModTime: time.Unix(file.MTime, 0), Typeflag: tar.TypeReg, Format: tar.FormatGNU})
		tw.Write([]byte(file.Content))
	}
	extra := f.TarExtra
	f.mu.Unlock()
	if extra != nil {
		extra(tw)
	}
	tw.Close()
	out := buf.String()
	if cut > 0 && cut < len(out) {
		return out[:cut], &phone.ExitError{Code: 1, Stderr: "error: device offline"}
	}
	if missing {
		return out, &phone.ExitError{Code: 1}
	}
	return out, nil
}

// parseTarCommand reads "cd <root> && tar -cf - -- <rels...> 2>/dev/null".
func parseTarCommand(cmd string) (root string, rels []string, ok bool) {
	words := shellWords(cmd)
	if len(words) < 7 || words[0] != "cd" || words[2] != "&&" || words[3] != "tar" || words[4] != "-cf" || words[5] != "-" || words[6] != "--" {
		return "", nil, false
	}
	root = words[1]
	for _, w := range words[7:] {
		if w == "2>/dev/null" {
			break
		}
		rels = append(rels, w)
	}
	return root, rels, true
}

// shellWords splits a command line the way the phone's shell would, for
// the quoting DriveDex produces: bare words and '...' with '\” inside.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
