package phone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Caps is what this phone's shell supports. Phones differ: Android ships
// "toybox" (or, on old or custom phones, "busybox") versions of find and tar,
// and not every version knows every option. DriveDex asks once per phone
// (Probe) instead of assuming.
type Caps struct {
	// Tar: the phone's tar can create an archive and write it to stdout (stage 16).
	Tar bool
	// FindCmin: find understands -cmin, so "list only what changed in the
	// last N minutes" works (stage 15).
	FindCmin bool
	// PullCompress lists the compression algorithms that BOTH this adb
	// program and the phone support for 'adb pull -z', best first (stage 16).
	PullCompress []string
}

// CminProbeCommand asks the phone's find whether it knows -cmin. "/" always
// exists; -maxdepth 0 keeps it to that one entry, so it is instant. stderr is
// folded into stdout and the exit status printed, because older adb versions
// don't pass the remote exit status back.
const CminProbeCommand = `find / -maxdepth 0 -cmin -1 2>&1; echo DDX_RC=$?`

// ParseCminProbe decides from the probe's output whether -cmin works: the
// command must have exited with 0 and not complained about the option.
func ParseCminProbe(out string) bool {
	low := strings.ToLower(out)
	if !strings.Contains(low, "ddx_rc=0") {
		return false
	}
	for _, bad := range []string{"unknown", "bad arg", "invalid", "usage", "unrecognized", "not found"} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	return true
}

// Probe returns what the phone with the given serial supports. The answer
// is cached for the life of this ADB (one probe per phone per run).
func (a *ADB) Probe(ctx context.Context, serial string) (Caps, error) {
	a.capsMu.Lock()
	if c, ok := a.caps[serial]; ok {
		a.capsMu.Unlock()
		return c, nil
	}
	a.capsMu.Unlock()

	var c Caps
	out, err := a.run.Output(ctx, "-s", serial, "shell", CminProbeCommand)
	if ctx.Err() != nil {
		return Caps{}, ctx.Err()
	}
	var ee *ExitError
	if err != nil && !errors.As(err, &ee) {
		return Caps{}, fmt.Errorf("asking the phone what it supports: %w", err)
	}
	c.FindCmin = ParseCminProbe(string(out))
	c.Tar = a.probeTar(ctx, serial)
	c.PullCompress = a.probeCompress(ctx, serial)
	if ctx.Err() != nil {
		return Caps{}, ctx.Err()
	}

	a.capsMu.Lock()
	if a.caps == nil {
		a.caps = map[string]Caps{}
	}
	a.caps[serial] = c
	a.capsMu.Unlock()
	return c, nil
}

// DeltaCommand lists the files under root whose status changed in the last
// `minutes` minutes, in the same "size|mtime|path" format as StatCommand, so
// the same parser reads it.
//
// It uses -cmin (change time), not -mmin (modification time): a file that was
// moved or renamed keeps its modification time but gets a new change time,
// so -mmin would miss it.
func DeltaCommand(root string, minutes int) string {
	if minutes < 1 {
		minutes = 1
	}
	return `find ` + shellQuote(root) + ` -type f -cmin -` + strconv.Itoa(minutes) + ` -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`
}

// RemoteFile is one file on the phone.
type RemoteFile struct {
	Path    string // absolute path on the phone
	Size    int64
	ModTime int64 // unix seconds
}

// ListChanged runs DeltaCommand on the phone and returns the files it printed.
// As with a full listing, a non-zero exit (some folder was unreadable) is
// fine as long as output arrived.
func (a *ADB) ListChanged(ctx context.Context, serial, root string, minutes int) ([]RemoteFile, error) {
	if !strings.HasSuffix(root, "/") {
		root += "/" // follow a symlinked root such as /sdcard
	}
	stdout, wait, err := a.run.Stream(ctx, "-s", serial, "shell", DeltaCommand(root, minutes))
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	var files []RemoteFile
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for lines.Scan() {
		if size, mtime, p, ok := ParseStatFields(lines.Text()); ok {
			files = append(files, RemoteFile{Path: p, Size: size, ModTime: mtime})
		}
	}
	scanErr := lines.Err()
	waitErr := wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		return nil, fmt.Errorf("reading the list of changed files: %w", scanErr)
	}
	var ee *ExitError
	if waitErr != nil && !errors.As(waitErr, &ee) {
		return nil, waitErr
	}
	return files, nil
}

// ParseStatFields splits one "size|mtime|path" line. The path is last and may
// itself contain "|".
func ParseStatFields(line string) (size, mtime int64, path string, ok bool) {
	line = strings.TrimRight(line, "\r")
	parts := strings.SplitN(line, "|", 3)
	if len(parts) != 3 || parts[2] == "" {
		return 0, 0, "", false
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || size < 0 {
		return 0, 0, "", false
	}
	return size, mtime, parts[2], true
}
