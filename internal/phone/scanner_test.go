package phone_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
	"github.com/yashwardhanv/dd/internal/scan"
)

func sampleOutput(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/adb_find_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// scanPhone runs the scanner against a fake phone that answers the stat and
// names commands as given. exit is the error `adb shell` ends with.
func scanPhone(t *testing.T, handler func(ctx context.Context, args []string) (string, error)) (*phone.Scanner, *index.Index, *phonetest.Fake, error) {
	t.Helper()
	adb, fake := phonetest.New(handler)
	sc := phone.NewScanner(adb, "R58M1234ABC", "")
	b := index.NewBuilder(index.Header{})
	err := sc.Scan(context.Background(), b, nil)
	return sc, b.Index(), fake, err
}

func TestScannerSendsTheExactRemoteCommandAsOneArgument(t *testing.T) {
	out := sampleOutput(t)
	_, _, fake, err := scanPhone(t, func(context.Context, []string) (string, error) { return out, nil })
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	want := []string{"-s", "R58M1234ABC", "shell", `find /sdcard/ -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`}
	if strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("adb args = %q\nwant       %q", calls[0], want)
	}
}

func TestScannerIndexesTheSampleAndToleratesFindExitStatus(t *testing.T) {
	out := sampleOutput(t)
	// find exits 1 when some folder was unreadable (Android/data...) yet printed everything else.
	sc, ix, fake, err := scanPhone(t, func(context.Context, []string) (string, error) {
		return out, &phone.ExitError{Code: 1}
	})
	if err != nil {
		t.Fatalf("non-zero exit WITH results must be accepted: %v", err)
	}
	if len(fake.Calls()) != 1 || sc.NamesOnly() {
		t.Errorf("must not fall back when results arrived: calls=%d namesOnly=%v", len(fake.Calls()), sc.NamesOnly())
	}
	st := sc.Stats()
	if st.Files != 8 || st.Dirs != 12 || st.Errors != 4 || len(st.FirstErrors) != 4 {
		t.Errorf("stats = %+v", st)
	}
	if ix.Len() != 20 {
		t.Errorf("index has %d entries, want 20", ix.Len())
	}
	if sc.Name() != "adb" {
		t.Errorf("Name = %q", sc.Name())
	}
	var _ scan.Scanner = sc
	var _ scan.StatsProvider = sc
}

func TestScannerFallsBackToNamesWhenStatFails(t *testing.T) {
	names := "/sdcard/DCIM/a.jpg\n/sdcard/Download/b.pdf\n/sdcard/c.txt\n"
	sc, ix, fake, err := scanPhone(t, func(_ context.Context, args []string) (string, error) {
		if strings.Contains(args[len(args)-1], "stat -c") {
			return "", &phone.ExitError{Code: 1} // this phone's stat doesn't know -c
		}
		return names, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sc.NamesOnly() || len(fake.Calls()) != 2 {
		t.Fatalf("namesOnly=%v calls=%d", sc.NamesOnly(), len(fake.Calls()))
	}
	if got := fake.Calls()[1][3]; got != `find /sdcard/ -type f 2>/dev/null` {
		t.Errorf("fallback command = %q", got)
	}
	if sc.Stats().Files != 3 || sc.Stats().Dirs != 2 {
		t.Errorf("stats = %+v", sc.Stats())
	}
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Flags[slot]&index.FlagDir == 0 && (ix.Sizes[slot] != -1 || ix.MTimes[slot] != 0 || ix.Flags[slot]&index.FlagNoMeta == 0) {
			t.Errorf("%s: names-only entries need Size=-1, ModTime=0, FlagNoMeta", ix.Names[slot])
		}
	}
}

func TestScannerFallsBackWhenOutputIsAllGarbage(t *testing.T) {
	sc, _, _, err := scanPhone(t, func(_ context.Context, args []string) (string, error) {
		if strings.Contains(args[len(args)-1], "stat -c") {
			return "stat: invalid option -- 'c'\nusage: ...\n", nil // exit 0 but useless
		}
		return "/sdcard/x.txt\n", nil
	})
	if err != nil || !sc.NamesOnly() || sc.Stats().Files != 1 {
		t.Errorf("err=%v namesOnly=%v stats=%+v", err, sc.NamesOnly(), sc.Stats())
	}
	// The garbage from the first attempt must not be counted against the fallback.
	if sc.Stats().Errors != 0 {
		t.Errorf("Errors = %d, want 0 after clean fallback", sc.Stats().Errors)
	}
}

func TestScannerFailsClearlyWhenNothingWorks(t *testing.T) {
	_, _, _, err := scanPhone(t, func(context.Context, []string) (string, error) {
		return "", &phone.ExitError{Code: 1, Stderr: "error: device unauthorized."}
	})
	var ee *phone.ExitError
	if err == nil || !errors.As(err, &ee) || !strings.Contains(err.Error(), "no files") {
		t.Errorf("err = %v", err)
	}
}

func TestScannerAcceptsAnEmptyPhone(t *testing.T) {
	sc, ix, fake, err := scanPhone(t, func(context.Context, []string) (string, error) { return "", nil })
	if err != nil || ix.Len() != 0 || sc.NamesOnly() || len(fake.Calls()) != 1 {
		t.Errorf("err=%v len=%d namesOnly=%v calls=%d", err, ix.Len(), sc.NamesOnly(), len(fake.Calls()))
	}
}

func TestScannerReportsAdbStartFailure(t *testing.T) {
	adb := phone.NewADBWithRunner(failingRunner{})
	err := phone.NewScanner(adb, "S", "").Scan(context.Background(), index.NewDiscardBuilder(), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot start adb") {
		t.Errorf("err = %v", err)
	}
}

func TestScannerStopsOnCancellation(t *testing.T) {
	adb, _ := phonetest.New(func(ctx context.Context, args []string) (string, error) {
		<-ctx.Done() // a phone that never answers
		return "", ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	done := make(chan error, 1)
	go func() { done <- phone.NewScanner(adb, "S", "").Scan(ctx, index.NewDiscardBuilder(), nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not stop")
	}
}

func TestScannerHandlesVeryLongLines(t *testing.T) {
	long := strings.Repeat("x", 200_000) // far beyond bufio's default 64 KB limit
	out := "5|10|/sdcard/" + long + ".txt\n"
	sc, _, _, err := scanPhone(t, func(context.Context, []string) (string, error) { return out, nil })
	if err != nil || sc.Stats().Files != 1 {
		t.Errorf("err=%v stats=%+v", err, sc.Stats())
	}
	tooLong := "5|10|/sdcard/" + strings.Repeat("y", 1<<20+10) + "\n"
	if _, _, _, err := scanPhone(t, func(context.Context, []string) (string, error) { return tooLong, nil }); err == nil {
		t.Error("a line over 1 MB should be an error, not silently dropped")
	}
}

func TestScannerReportsProgress(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("1|1|/sdcard/f")
		sb.WriteString(strings.Repeat("a", i%7))
		sb.WriteString(string(rune('0' + i%10)))
		sb.WriteString("/file")
		sb.WriteString(strings.Repeat("b", i%50))
		sb.WriteString(".txt\n")
	}
	adb, _ := phonetest.New(func(context.Context, []string) (string, error) { return sb.String(), nil })
	calls := 0
	var last scan.Progress
	err := phone.NewScanner(adb, "S", "").Scan(context.Background(), index.NewDiscardBuilder(), func(p scan.Progress) { calls++; last = p })
	if err != nil || calls < 2 || last.Entries < 2000 {
		t.Errorf("err=%v calls=%d last=%+v", err, calls, last)
	}
}

func TestStatCommandQuotesAwkwardRoots(t *testing.T) {
	cases := map[string]string{
		"/sdcard/":                 `find /sdcard/ -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`,
		"/sdcard/DCIM/Camera":      `find /sdcard/DCIM/Camera -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`,
		"/sdcard/My Photos":        `find '/sdcard/My Photos' -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`,
		"/sdcard/it's here":        `find '/sdcard/it'\''s here' -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`,
		"/sdcard/$(rm -rf x);`id`": `find '/sdcard/$(rm -rf x);` + "`id`" + `' -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`,
	}
	for root, want := range cases {
		if got := phone.StatCommand(root); got != want {
			t.Errorf("StatCommand(%q)\n got %s\nwant %s", root, got, want)
		}
	}
}

// failingRunner can't even start adb.
type failingRunner struct{}

func (failingRunner) Output(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("cannot start adb")
}
func (failingRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("cannot start adb")
}
