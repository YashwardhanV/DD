package phone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// DefaultRoot is the phone's shared storage. (The trailing slash matters: it
// makes `find` follow the /sdcard symlink into the real storage folder.)
const DefaultRoot = "/sdcard/"

// maxLineBytes is the longest line we accept from the phone (a path can be
// long, but never this long).
const maxLineBytes = 1 << 20

// Scanner indexes a phone. It implements scan.Scanner (Name() == "adb").
type Scanner struct {
	ADB    *ADB
	Serial string
	Root   string // remote folder to list; "" means DefaultRoot

	stats    scan.Stats
	fellBack bool
}

// NewScanner returns a scanner for the phone with the given serial.
func NewScanner(adb *ADB, serial, root string) *Scanner {
	return &Scanner{ADB: adb, Serial: serial, Root: root}
}

// Name implements scan.Scanner.
func (s *Scanner) Name() string { return "adb" }

// Stats returns the tally of the most recent Scan. Errors counts lines the
// phone printed that couldn't be understood.
func (s *Scanner) Stats() scan.Stats { return s.stats }

// NamesOnly reports whether the last scan had to fall back to a listing
// without sizes and dates (the phone's `stat` didn't work).
func (s *Scanner) NamesOnly() bool { return s.fellBack }

func (s *Scanner) root() string {
	if s.Root == "" {
		return DefaultRoot
	}
	return s.Root
}

// shellQuote makes text safe to put inside the phone's shell command line.
func shellQuote(p string) string {
	safe := true
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./-", r)) {
			safe = false
			break
		}
	}
	if safe && p != "" {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// StatCommand is the remote command that lists every file with its size and
// modification time: one line per file, "size|mtime|path". The phone runs it
// locally, so there is no per-folder round trip over USB.
//
//   - `-exec stat ... {} +` runs stat on many files per call (fast).
//   - `2>/dev/null` hides "permission denied" for folders Android protects
//     (such as Android/data on Android 11+).
//   - The path comes last so a name containing "|" still parses.
func StatCommand(root string) string {
	return `find ` + shellQuote(root) + ` -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`
}

// NamesCommand is the fallback when stat is unavailable: names only.
func NamesCommand(root string) string {
	return `find ` + shellQuote(root) + ` -type f 2>/dev/null`
}

// Scan implements scan.Scanner.
func (s *Scanner) Scan(ctx context.Context, b *index.Builder, report func(scan.Progress)) error {
	s.stats = scan.Stats{}
	s.fellBack = false

	parser := NewParser(b, s.root())
	err := s.run(ctx, StatCommand(s.root()), parser.ParseStatLine, parser, report)
	var ee *ExitError
	switch {
	case err == nil || errors.As(err, &ee):
		// `find` exits non-zero when ANY folder was unreadable, even though it
		// printed everything else - so a non-zero exit with results is fine.
	default:
		return err
	}

	if parser.Files == 0 && (err != nil || parser.Malformed > 0) {
		// Nothing usable came back: probably this phone's stat doesn't support -c.
		// Retry for names only. Start again from a clean slate.
		s.fellBack = true
		*parser = *NewParser(b, s.root())
		err = s.run(ctx, NamesCommand(s.root()), parser.ParseNameLine, parser, report)
		if err != nil && !errors.As(err, &ee) {
			return err
		}
		if parser.Files == 0 && err != nil {
			return fmt.Errorf("the phone returned no files: %w", err)
		}
	}

	s.stats = scan.Stats{Files: parser.Files, Dirs: parser.Dirs, Errors: parser.Malformed}
	for _, l := range parser.FirstMalformed {
		s.stats.FirstErrors = append(s.stats.FirstErrors, "unreadable line: "+l)
	}
	return nil
}

// run executes one remote command and feeds its output lines to parse.
// It returns the command's exit error (if any) AFTER all output was read.
func (s *Scanner) run(ctx context.Context, remote string, parse func(string) bool, parser *Parser, report func(scan.Progress)) error {
	// The whole remote command is ONE argument so Windows quoting can't mangle it.
	stdout, wait, err := s.ADB.run.Stream(ctx, "-s", s.Serial, "shell", remote)
	if err != nil {
		return err
	}
	defer stdout.Close()

	var last int64
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for lines.Scan() {
		if ctx.Err() != nil {
			break
		}
		parse(lines.Text())
		if report != nil {
			if n := parser.Entries(); n-last >= 512 { // cheap throttle; the CLI also rate-limits drawing
				last = n
				report(scan.Progress{Entries: n, Dirs: parser.Dirs, Errors: parser.Malformed})
			}
		}
	}
	scanErr := lines.Err()
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		stdout.Close()
	}
	waitErr := wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return fmt.Errorf("reading the phone's file list (%s): %w", parser.summary(), scanErr)
	}
	if report != nil {
		report(scan.Progress{Entries: parser.Entries(), Dirs: parser.Dirs, Errors: parser.Malformed})
	}
	return waitErr
}
