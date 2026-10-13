// Package agent is the background program that keeps indexes fresh by itself:
// it notices when an external drive, a phone or another portable device is plugged in and re-indexes
// it, so the owner never has to remember to run `drivedex index`.
//
// The package is split so that nearly everything can be tested on any machine:
//
//   - watch.go    the decision logic: what counts as "a drive just arrived"
//   - lock.go     the lock file that stops two programs indexing one drive
//   - log.go      the log file (with rotation)
//   - notify.go   the Windows pop-up message
//   - instance_*  the Windows-only parts (single instance, process control)
//   - startup.go  starting the agent when you log in
package agent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultMaxLogBytes is when the log file is rotated (5 MB).
const DefaultMaxLogBytes = 5 << 20

// Logger writes one line per event to a file, and keeps the file from growing
// forever: when it reaches maxBytes it is renamed to "<name>.1" (replacing the
// previous .1) and a fresh file is started.
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
	now      func() time.Time
}

// NewLogger opens (creating the folder if needed) the log file at path.
func NewLogger(path string) (*Logger, error) {
	l := &Logger{path: path, maxBytes: DefaultMaxLogBytes, now: time.Now}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the log folder: %w", err)
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log %s: %w", l.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, st.Size()
	return nil
}

// Close closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Infof logs an event.
func (l *Logger) Infof(format string, args ...any) { l.write("INFO ", fmt.Sprintf(format, args...)) }

// Errorf logs a problem.
func (l *Logger) Errorf(format string, args ...any) { l.write("ERROR", fmt.Sprintf(format, args...)) }

func (l *Logger) write(level, msg string) {
	if l == nil {
		return
	}
	// One line per event: flatten any line breaks in the message.
	msg = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(msg)
	line := fmt.Sprintf("%s %s %s\n", l.now().Format("2006-01-02 15:04:05"), level, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if l.size+int64(len(line)) > l.maxBytes && l.size > 0 {
		l.rotate()
		if l.file == nil {
			return
		}
	}
	n, _ := l.file.WriteString(line)
	l.size += int64(n)
}

// rotate moves the current log to "<name>.1" and starts a new one.
func (l *Logger) rotate() {
	l.file.Close()
	l.file = nil
	os.Remove(l.path + ".1")
	os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		l.file = nil // logging is best-effort: never crash the agent over it
	}
}

// TailLines returns the last n lines of a log file (fewer if the file is
// short). A missing file gives no lines and no error.
func TailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Only the end of the file matters: read at most the last 64 KB.
	const window = 64 << 10
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > window {
		if _, err := f.Seek(st.Size()-window, io.SeekStart); err != nil {
			return nil, err
		}
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if st.Size() > window && len(lines) > 0 {
		lines = lines[1:] // the first line is probably cut in half
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// firstLineOf returns the first line of s (for compact error messages).
func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
