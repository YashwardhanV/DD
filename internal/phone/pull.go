package phone

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PathInfo describes one path on the phone.
type PathInfo struct {
	Exists  bool
	IsDir   bool
	Size    int64
	ModTime int64 // unix seconds
}

// StatPath asks the phone about one path ("does it exist, is it a folder?").
func (a *ADB) StatPath(ctx context.Context, serial, remote string) (PathInfo, error) {
	cmd := `stat -c '%F|%s|%Y' ` + shellQuote(remote) + ` 2>/dev/null`
	out, err := a.run.Output(ctx, "-s", serial, "shell", cmd)
	var ee *ExitError
	if err != nil && !errors.As(err, &ee) {
		return PathInfo{}, err
	}
	line := strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	parts := strings.SplitN(line, "|", 3)
	if len(parts) != 3 {
		return PathInfo{}, nil // nothing printed: the path doesn't exist
	}
	size, err1 := strconv.ParseInt(parts[1], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return PathInfo{}, nil
	}
	return PathInfo{Exists: true, IsDir: strings.Contains(parts[0], "directory"), Size: size, ModTime: mtime}, nil
}

// Pull copies files from the phone into a local folder with `adb pull -a`
// (which keeps their modification times). All remotes land directly in
// localDir under their own names.
func (a *ADB) Pull(ctx context.Context, serial string, remotes []string, localDir string) error {
	if len(remotes) == 0 {
		return nil
	}
	args := append([]string{"-s", serial, "pull", "-a"}, remotes...)
	args = append(args, localDir)
	if _, err := a.run.Output(ctx, args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("adb pull: %w", err)
	}
	return nil
}

// PullFile copies ONE file from the phone to the local file path localFile
// (which may have a different name than on the phone) with `adb pull -a`.
func (a *ADB) PullFile(ctx context.Context, serial, remote, localFile string) error {
	if _, err := a.run.Output(ctx, "-s", serial, "pull", "-a", remote, localFile); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("adb pull: %w", err)
	}
	return nil
}
