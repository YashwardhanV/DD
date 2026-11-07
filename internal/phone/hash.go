package phone

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"strings"
)

// Hash tools, in order of preference. Modern Android ships all three in
// toybox; very old phones may have none.
var hashTools = []string{"sha256sum", "sha1sum", "md5sum"}

// HashToolCommand prints the name of the first hash tool the phone has.
const HashToolCommand = `for t in sha256sum sha1sum md5sum; do if command -v $t >/dev/null 2>&1; then echo $t; break; fi; done`

// HashTool asks the phone which checksum program it has ("sha256sum",
// "sha1sum" or "md5sum"), or "" if none. Ask once per connected phone.
func (a *ADB) HashTool(ctx context.Context, serial string) (string, error) {
	out, err := a.run.Output(ctx, "-s", serial, "shell", HashToolCommand)
	var ee *ExitError
	if err != nil && !errors.As(err, &ee) {
		return "", err
	}
	got := strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
	if i := strings.IndexByte(got, '\n'); i >= 0 {
		got = got[:i]
	}
	for _, t := range hashTools {
		if got == t {
			return t, nil
		}
	}
	return "", nil
}

// NewHash returns the PC-side hash that matches a phone hash tool.
func NewHash(tool string) (hash.Hash, bool) {
	switch tool {
	case "sha256sum":
		return sha256.New(), true
	case "sha1sum":
		return sha1.New(), true
	case "md5sum":
		return md5.New(), true
	}
	return nil, false
}

// HashCommand builds the shell command that hashes paths on the phone with
// tool. The list is fed to xargs NUL-separated (printf '%s\0'), so any file
// name, even with spaces or quotes, reaches the tool intact, and xargs runs it
// on as many files per call as fit.
func HashCommand(tool string, paths []string) string {
	var sb strings.Builder
	sb.WriteString(`printf '%s\0'`)
	for _, p := range paths {
		sb.WriteByte(' ')
		sb.WriteString(shellQuote(p))
	}
	sb.WriteString(" | xargs -0 ")
	sb.WriteString(tool)
	sb.WriteString(" 2>/dev/null")
	return sb.String()
}

// HashFiles hashes paths on the phone (without copying them) and returns
// path -> lowercase hex digest. Files the tool couldn't read are missing from
// the map. Callers batch paths (a few hundred at most per call) to stay well
// under the command-line limit.
func (a *ADB) HashFiles(ctx context.Context, serial, tool string, paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	if _, ok := NewHash(tool); !ok {
		return nil, errors.New("unknown hash tool " + tool)
	}
	out, err := a.run.Output(ctx, "-s", serial, "shell", HashCommand(tool, paths))
	var ee *ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, err // a non-zero exit only means some file couldn't be read
	}
	return ParseHashOutput(string(out)), nil
}

// ParseHashOutput reads "<hex>  <path>" lines as printed by sha256sum and
// friends. Lines whose path the tool had to escape (they start with '\',
// for names containing a newline or backslash) are left out: such files are
// simply not verified, never wrongly matched.
func ParseHashOutput(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if line == "" || line[0] == '\\' {
			continue
		}
		i := strings.Index(line, "  ")
		if i <= 0 {
			continue
		}
		sum := strings.ToLower(line[:i])
		if _, err := hex.DecodeString(sum); err != nil || (len(sum) != 32 && len(sum) != 40 && len(sum) != 64) {
			continue
		}
		res[line[i+2:]] = sum
	}
	return res
}
