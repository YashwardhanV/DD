// Package install holds the pieces of `drivedex install` / `uninstall`:
// copying the program into its own folder, putting that folder on the user's
// PATH, and removing everything again.
//
// The logic that decides things (is the folder already on PATH? which files
// belong to DriveDex?) is plain code that is tested anywhere. The few lines
// that touch Windows (the registry value, the "settings changed" broadcast,
// deleting the running program after it exits) are in the *_windows.go files.
//
// Safety rules baked in: DriveDex only ever deletes files it knows it
// installed or created (by exact name), never a whole folder tree, and removes
// a folder only when it is empty.
package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---- PATH ---------------------------------------------------------------------------------

// PathStore reads and writes the current user's PATH variable.
type PathStore interface {
	// Get returns the raw value: %VARIABLES% are left as the user typed them.
	Get() (string, error)
	// Set stores the value and tells running programs that settings changed.
	Set(value string) error
}

var percentVar = regexp.MustCompile(`%([^%]+)%`)

// ExpandPercent replaces %NAME% with the environment variable's value (the way
// Windows reads a PATH entry). Unknown variables are left as they are.
func ExpandPercent(s string) string {
	return percentVar.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := os.LookupEnv(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
}

// normalize makes two spellings of the same folder compare equal: case,
// surrounding spaces/quotes, `/` vs `\`, and a trailing slash don't matter.
func normalize(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"`)
	p = strings.ReplaceAll(ExpandPercent(p), "/", `\`)
	p = strings.TrimRight(p, `\`)
	return strings.ToLower(p)
}

// SamePath reports whether a and b name the same folder (see normalize).
func SamePath(a, b string) bool { return normalize(a) == normalize(b) }

// PathContains reports whether dir is one of the entries of a PATH value.
func PathContains(path, dir string) bool {
	for _, e := range strings.Split(path, ";") {
		if strings.TrimSpace(e) != "" && SamePath(e, dir) {
			return true
		}
	}
	return false
}

// PathAdd appends dir to a PATH value unless it is already there. Everything
// already in the value is kept exactly as it was.
func PathAdd(path, dir string) (string, bool) {
	if PathContains(path, dir) {
		return path, false
	}
	switch {
	case strings.TrimSpace(path) == "":
		return dir, true
	case strings.HasSuffix(path, ";"):
		return path + dir, true
	}
	return path + ";" + dir, true
}

// PathRemove removes every entry equal to dir. The other entries are kept
// exactly as they were.
func PathRemove(path, dir string) (string, bool) {
	parts := strings.Split(path, ";")
	kept := parts[:0:0]
	for _, e := range parts {
		if strings.TrimSpace(e) != "" && SamePath(e, dir) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == len(parts) {
		return path, false
	}
	return strings.Join(kept, ";"), true
}

// AddToUserPath puts dir on the user's PATH. changed is false if it already was.
func AddToUserPath(ps PathStore, dir string) (changed bool, err error) {
	cur, err := ps.Get()
	if err != nil {
		return false, fmt.Errorf("reading your PATH: %w", err)
	}
	next, changed := PathAdd(cur, dir)
	if !changed {
		return false, nil
	}
	if err := ps.Set(next); err != nil {
		return false, fmt.Errorf("saving your PATH: %w", err)
	}
	return true, nil
}

// RemoveFromUserPath takes dir off the user's PATH. changed is false if it wasn't there.
func RemoveFromUserPath(ps PathStore, dir string) (changed bool, err error) {
	cur, err := ps.Get()
	if err != nil {
		return false, fmt.Errorf("reading your PATH: %w", err)
	}
	next, changed := PathRemove(cur, dir)
	if !changed {
		return false, nil
	}
	if err := ps.Set(next); err != nil {
		return false, fmt.Errorf("saving your PATH: %w", err)
	}
	return true, nil
}

// ---- program files ---------------------------------------------------------------------

// Program files DriveDex installs (and is allowed to delete again).
var (
	ExeName      = "drivedex.exe"
	AgentExeName = "drivedex-agent.exe"
)

// ProgramFiles are the exact names of the files install puts in the program
// folder. Uninstall deletes these and nothing else.
func ProgramFiles() []string { return []string{ExeName, AgentExeName} }

// leftovers are temporary names a copy can leave behind if it was interrupted.
func leftovers() []string {
	var out []string
	for _, n := range ProgramFiles() {
		out = append(out, n+".new", n+".old")
	}
	return out
}

// CopyProgram copies the named files from srcDir to dstDir (creating dstDir).
// Each file is written next to its final name and then renamed over it, so a
// failed copy never leaves a half-written program behind.
func CopyProgram(srcDir, dstDir string, names []string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dstDir, err)
	}
	for _, name := range names {
		if err := copyOne(filepath.Join(srcDir, name), filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyOne(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s: %w", filepath.Base(src), err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	// On Windows this fails if the old program is running; the caller says so.
	if err = os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("replacing %s: %w", dst, err)
	}
	return nil
}

// ErrInUse wraps errors that probably mean "that program is still running".
var ErrInUse = errors.New("the program is in use")

// IsInUse guesses whether err is Windows refusing to replace/delete a running program.
func IsInUse(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "being used by another process") || strings.Contains(s, "access is denied") || strings.Contains(s, "permission denied")
}

// RemoveProgram deletes the program files (and interrupted-copy leftovers) from
// dir, then dir itself if - and only if - it is now empty. It returns the
// names of files that could not be removed or that were not DriveDex's own.
func RemoveProgram(dir string) (left []string, err error) {
	for _, name := range append(ProgramFiles(), leftovers()...) {
		if rmErr := os.Remove(filepath.Join(dir, name)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			left = append(left, name)
			err = errors.Join(err, rmErr)
		}
	}
	if len(left) > 0 {
		return left, err
	}
	if rmErr := os.Remove(dir); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		// Not empty (the user put something there) or in use: leave it, say what remains.
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			left = append(left, e.Name())
		}
	}
	return left, nil
}

// ProgramPresent reports whether any program file is in dir.
func ProgramPresent(dir string) bool {
	for _, name := range ProgramFiles() {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// ---- DriveDex's own data -----------------------------------------------------------------

// DataFile is a file DriveDex itself created.
type DataFile struct {
	Path string
	Size int64
}

// OwnedData lists the files DriveDex created in its data folders: saved
// indexes (and their lock/temp files), the agent's log and PID file, and the
// settings file. It never lists anything else, so deleting exactly this list
// can't touch a user's files even if a folder is shared.
func OwnedData(indexDir, dataDir, configPath string) []DataFile {
	var out []DataFile
	add := func(path string) {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			out = append(out, DataFile{Path: path, Size: st.Size()})
		}
	}
	if entries, err := os.ReadDir(indexDir); err == nil {
		for _, e := range entries {
			n := strings.ToLower(e.Name())
			if strings.HasSuffix(n, ".ddx") || strings.HasSuffix(n, ".ddx.tmp") || strings.HasSuffix(n, ".lock") {
				add(filepath.Join(indexDir, e.Name()))
			}
		}
	}
	if dataDir != "" {
		for _, n := range []string{"agent.log", "agent.log.1", "agent.pid"} {
			add(filepath.Join(dataDir, n))
		}
	}
	if configPath != "" {
		add(configPath)
	}
	return out
}

// RemoveOwnedData deletes the listed files, then removes each of dirs if it is
// empty. A folder with anything else in it stays.
func RemoveOwnedData(files []DataFile, dirs ...string) error {
	var errs error
	for _, f := range files {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = errors.Join(errs, err)
		}
	}
	for _, d := range dirs {
		if d != "" {
			os.Remove(d) // fails harmlessly unless the folder is empty
		}
	}
	return errs
}

// removalCommandLine builds the cmd.exe command line that deletes paths and then
// removes dir if it is empty, after a short pause (so the program that started
// it has exited and its .exe is no longer locked).
//
//	cmd.exe /d /s /c "ping -n 4 127.0.0.1 >nul & del /f /q "A" "B" & rmdir "D""
//
// /s makes cmd strip exactly the outer pair of quotes. Inside quotes cmd treats
// & ^ | < > as plain text; the only characters that still do something are %
// (variable expansion) and line breaks, so a path containing those is refused
// (the caller then tells the user to delete the folder by hand).
func removalCommandLine(dir string, paths []string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) && !strings.HasPrefix(dir, `\\`) || filepath.Dir(dir) == dir {
		return "", fmt.Errorf("refusing to schedule the removal of %q: not a program folder", dir)
	}
	quote := func(p string) (string, error) {
		if strings.ContainsAny(p, "\"%\r\n") {
			return "", fmt.Errorf("the path %q has a character (\" or %%) that can't be handled safely - delete it by hand", p)
		}
		return `"` + p + `"`, nil
	}
	var qs []string
	for _, p := range paths {
		q, err := quote(p)
		if err != nil {
			return "", err
		}
		qs = append(qs, q)
	}
	qd, err := quote(dir)
	if err != nil {
		return "", err
	}
	return `cmd.exe /d /s /c "ping -n 4 127.0.0.1 >nul & del /f /q ` + strings.Join(qs, " ") + ` & rmdir ` + qd + `"`, nil
}
