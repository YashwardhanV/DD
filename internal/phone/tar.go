package phone

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Why tar (BUILD-V2 stage 16)
//
// 'adb pull' moves files one at a time: for every file the PC asks, the phone
// answers with the file, and the PC acknowledges - a round trip per file even
// when 100 files are named in one command. With thousands of small files
// (WhatsApp media, screenshots) the USB link spends most of its time waiting.
//
// tar packs many files back to back into ONE continuous stream. The phone's
// own tar writes it to stdout, 'adb exec-out' carries it to the PC byte for
// byte, and DriveDex unpacks it as it arrives. The phone never waits for the
// PC between files.
//
// DriveDex stays read-only on the phone: "tar -c" only CREATES an archive -
// and writes it to stdout, not to a file on the phone.

// TarProbeCommand creates a tiny archive of one file every Android phone has.
const TarProbeCommand = "tar -cf - -C /system/etc hosts"

// ParseTarProbe reports whether out is a tar archive holding exactly one
// regular file named "hosts" - proof the phone's tar can write to stdout and
// that exec-out delivers binary data unchanged.
func ParseTarProbe(out []byte) bool {
	tr := tar.NewReader(bytes.NewReader(out))
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
		n++
		if n > 1 || CleanTarName(h.Name) != "hosts" || !IsRegular(h) {
			return false
		}
	}
	return n == 1
}

// IsRegular reports whether a tar entry is a plain file (not a folder, link,
// device...). '\x00' is the old spelling of "regular file".
func IsRegular(h *tar.Header) bool { return h.Typeflag == tar.TypeReg || h.Typeflag == '\x00' }

// CleanTarName normalizes an entry name: "./DCIM/a.jpg" -> "DCIM/a.jpg".
// Absolute names stay absolute and ".." stays visible, so callers can refuse them.
func CleanTarName(name string) string {
	if name == "" {
		return ""
	}
	abs := strings.HasPrefix(name, "/")
	c := path.Clean(name)
	if !abs {
		c = strings.TrimPrefix(c, "./")
	}
	return c
}

// SafeTarName reports whether a cleaned entry name stays inside the folder
// being copied: not absolute, no ".." part.
func SafeTarName(clean string) bool {
	if clean == "" || clean == "." || strings.HasPrefix(clean, "/") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// TarCommand is the remote command that streams the given files (paths
// relative to root) as one tar archive on stdout. stderr is discarded so it
// can't be mixed into the binary stream.
func TarCommand(root string, rels []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(root))
	b.WriteString(" && tar -cf - --")
	for _, r := range rels {
		b.WriteByte(' ')
		b.WriteString(shellQuote(r))
	}
	b.WriteString(" 2>/dev/null")
	return b.String()
}

// QuotedLen is how many characters a path adds to a TarCommand.
func QuotedLen(rel string) int { return len(shellQuote(rel)) + 1 }

// TarStream runs "cd <root> && tar -cf - -- <rels...>" on the phone through
// "adb exec-out" and returns the raw tar stream. exec-out (unlike 'adb shell')
// passes binary data through untouched: older adb versions' shell mode turns
// every LF byte into CR LF, which would corrupt the files. The caller must
// read the stream to the end or close it, then call wait, which returns the
// exit error (tar exits non-zero when a requested file was missing).
func (a *ADB) TarStream(ctx context.Context, serial, root string, rels []string) (stream io.ReadCloser, wait func() error, err error) {
	return a.run.Stream(ctx, "-s", serial, "exec-out", TarCommand(root, rels))
}

// probeTar runs the tar probe.
func (a *ADB) probeTar(ctx context.Context, serial string) bool {
	out, err := a.run.Output(ctx, "-s", serial, "exec-out", TarProbeCommand)
	if err != nil {
		return false
	}
	return ParseTarProbe(out)
}

// Compression algorithms 'adb pull -z' can use, cheapest on the phone's CPU first.
var compressAlgos = []string{"lz4", "zstd", "brotli"}

// ParsePullCompress works out which algorithms both sides support: the adb
// program must mention "-z" in its help, and the phone must list the
// matching "sendrecv_v2_<algo>" feature.
func ParsePullCompress(help, features string) []string {
	if !strings.Contains(help, "-z") {
		return nil
	}
	have := map[string]bool{}
	for _, f := range strings.FieldsFunc(features, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' }) {
		have[strings.TrimSpace(f)] = true
	}
	var out []string
	for _, a := range compressAlgos {
		if have["sendrecv_v2_"+a] {
			out = append(out, a)
		}
	}
	return out
}

// probeCompress asks adb and the phone which pull compressions they share.
func (a *ADB) probeCompress(ctx context.Context, serial string) []string {
	help, err := a.run.Output(ctx, "help")
	text := string(help)
	var ee *ExitError
	if errors.As(err, &ee) {
		text += ee.Stderr // older adb versions print help to stderr
	}
	feats, err := a.run.Output(ctx, "-s", serial, "features")
	if err != nil {
		return nil
	}
	return ParsePullCompress(text, string(feats))
}

// PullCompressed is Pull with 'adb pull -z <algo>': the phone compresses each
// file on the fly. Only worth it for compressible files (text, databases);
// photos and videos are already compressed.
func (a *ADB) PullCompressed(ctx context.Context, serial string, remotes []string, localDir, algo string) error {
	if algo == "" {
		return a.Pull(ctx, serial, remotes, localDir)
	}
	if len(remotes) == 0 {
		return nil
	}
	args := append([]string{"-s", serial, "pull", "-a", "-z", algo}, remotes...)
	args = append(args, localDir)
	if _, err := a.run.Output(ctx, args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("adb pull -z %s: %w", algo, err)
	}
	return nil
}
