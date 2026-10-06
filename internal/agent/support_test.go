package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- log ------------------------------------------------------------------------------

func TestLoggerWritesOneLinePerEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agent.log")
	l, err := NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.Date(2026, 10, 6, 17, 55, 1, 0, time.UTC) }
	l.Infof("indexing %s", "E:")
	l.Errorf("multi\nline\r\nmessage")
	l.Close()

	data, _ := os.ReadFile(path)
	want := "2026-10-06 17:55:01 INFO  indexing E:\n2026-10-06 17:55:01 ERROR multi line message\n"
	if string(data) != want {
		t.Errorf("log =\n%q\nwant\n%q", data, want)
	}
	var nilLog *Logger
	nilLog.Infof("a nil logger must be harmless") // no panic
}

func TestLoggerRotatesAtTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	l, _ := NewLogger(path)
	l.maxBytes = 200
	defer l.Close()
	for i := 0; i < 40; i++ {
		l.Infof("event number %02d with some padding text to fill space", i)
	}
	cur, _ := os.ReadFile(path)
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotation must keep the previous log as agent.log.1: %v", err)
	}
	if len(cur) > 200+80 || len(old) == 0 {
		t.Errorf("current %d bytes, old %d bytes", len(cur), len(old))
	}
	if !strings.Contains(string(cur), "event number 39") {
		t.Error("the newest event must be in the current file")
	}
	// Only one old generation is kept.
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("there must be no agent.log.2")
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	if lines, err := TailLines(path, 10); err != nil || lines != nil {
		t.Errorf("a missing log is empty, not an error: %v %v", lines, err)
	}
	var sb strings.Builder
	for i := 1; i <= 25; i++ {
		sb.WriteString("line ")
		sb.WriteString(string(rune('A' + i%26)))
		sb.WriteString("\n")
	}
	os.WriteFile(path, []byte(sb.String()), 0o644)
	lines, _ := TailLines(path, 10)
	if len(lines) != 10 || lines[9] != "line Z" {
		t.Errorf("lines = %q", lines)
	}
	if lines, _ := TailLines(path, 100); len(lines) != 25 {
		t.Errorf("a short file returns everything: %d", len(lines))
	}
	// A big file: only the end is read, and the half-cut first line is dropped.
	big := strings.Repeat("0123456789abcdef-line-of-text\n", 10000)
	os.WriteFile(path, []byte(big+"LAST\n"), 0o644)
	lines, _ = TailLines(path, 3)
	if len(lines) != 3 || lines[2] != "LAST" || lines[0] != "0123456789abcdef-line-of-text" {
		t.Errorf("big file tail = %q", lines)
	}
}

// ---- locks ------------------------------------------------------------------------------

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vol-1.lock")
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocked(path) {
		t.Error("IsLocked should be true while held")
	}
	if _, err := AcquireLock(path); err != ErrLocked {
		t.Errorf("second acquire = %v, want ErrLocked", err)
	}
	release()
	release() // safe to call twice
	if _, err := os.Stat(path); err == nil || IsLocked(path) {
		t.Error("release must remove the lock file")
	}
	release2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("lock should be free again: %v", err)
	}
	release2()
}

func TestStaleLockIsTakenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vol-2.lock")
	os.WriteFile(path, []byte("pid 1\n"), 0o644)
	old := time.Now().Add(-StaleLockAge - time.Minute)
	os.Chtimes(path, old, old)
	if IsLocked(path) {
		t.Error("a lock older than 10 minutes isn't a lock")
	}
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("a stale lock must be taken over: %v", err)
	}
	release()

	// A recent lock is respected.
	os.WriteFile(path, []byte("pid 1\n"), 0o644)
	if _, err := AcquireLock(path); err != ErrLocked {
		t.Errorf("fresh lock: %v", err)
	}
}

func TestOnlyOneOfManyRacingProcessesWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vol-3.lock")
	var won, lost atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	var releases []func()
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if rel, err := AcquireLock(path); err == nil {
				won.Add(1)
				mu.Lock()
				releases = append(releases, rel)
				mu.Unlock()
			} else {
				lost.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won.Load() != 1 || lost.Load() != 19 {
		t.Errorf("won %d, lost %d", won.Load(), lost.Load())
	}
	for _, r := range releases {
		r()
	}
}

func TestAHolderKeepsItsLockFreshDuringALongScan(t *testing.T) {
	old := lockRefresh
	lockRefresh = 10 * time.Millisecond
	defer func() { lockRefresh = old }()

	path := filepath.Join(t.TempDir(), "vol-4.lock")
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// Pretend the file is ancient, as if a scan had been running for an hour...
	ancient := time.Now().Add(-time.Hour)
	os.Chtimes(path, ancient, ancient)
	// ...the holder touches it again, so nobody takes it over.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if IsLocked(path) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the holder must refresh its lock so a long scan isn't mistaken for a crash")
}

// ---- notifications -------------------------------------------------------------------------

func TestToastScriptQuotesTheTextSafely(t *testing.T) {
	s := ToastScript("SANDISK 32GB indexed", "182,334 entries")
	for _, want := range []string{"ToastText02", "CreateTextNode('SANDISK 32GB indexed')", "CreateTextNode('182,334 entries')", "CreateToastNotifier('" + powershellAppID + "')"} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// A hostile drive label must stay inside its string.
	evil := `x'); Remove-Item -Recurse C:\ ; ('`
	s = ToastScript(evil, "line1\nline2")
	if !strings.Contains(s, `CreateTextNode('x''); Remove-Item -Recurse C:\ ; (''')`) {
		t.Errorf("quote not doubled:\n%s", s)
	}
	if strings.Contains(s, "line1\nline2") || !strings.Contains(s, "line1 line2") {
		t.Error("line breaks must be flattened")
	}
	if got := psQuote("it's"); got != "'it''s'" {
		t.Errorf("psQuote = %q", got)
	}
}
