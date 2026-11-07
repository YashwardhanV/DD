package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// TestINV3_TrashMoveListRestore: a removed file is recoverable byte for byte.
func TestINV3_TrashMoveListRestore(t *testing.T) {
	dst := t.TempDir()
	write(t, dst, `Photos\a.jpg`, "AAA")
	write(t, dst, `Photos\Sub\b.jpg`, "BB")
	now := time.Date(2026, 11, 7, 18, 30, 0, 0, time.Local)
	r := Begin(dst, "sync", `E:\Photos`, now)
	if err := r.Move(`Photos\a.jpg`, Removed); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(`Photos\Sub\b.jpg`, Replaced); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if read(t, dst, `Photos\a.jpg`) != "<missing>" {
		t.Fatal("the file should have left its place")
	}

	runs, err := List(dst)
	if err != nil || len(runs) != 1 {
		t.Fatalf("List = %v, %v", runs, err)
	}
	if runs[0].ID != "20261107-183000" || runs[0].Files() != 2 || runs[0].Bytes() != 5 || runs[0].Kind != "sync" {
		t.Errorf("run = %+v", runs[0])
	}

	// Restore only the sub-folder, then everything.
	res, err := Restore(dst, runs[0].ID, `Photos\Sub`, nil, now)
	if err != nil || len(res.Restored) != 1 || read(t, dst, `Photos\Sub\b.jpg`) != "BB" {
		t.Fatalf("partial restore: %+v %v", res, err)
	}
	// Something new took a.jpg's place: restore never overwrites unasked.
	write(t, dst, `Photos\a.jpg`, "NEW")
	res, _ = Restore(dst, runs[0].ID, "", nil, now)
	if len(res.Existing) != 1 || read(t, dst, `Photos\a.jpg`) != "NEW" {
		t.Fatalf("restore must not overwrite: %+v", res)
	}
	// With permission the file in the way goes to a new trash run.
	res, _ = Restore(dst, runs[0].ID, "", func(string) bool { return true }, now.Add(time.Minute))
	if len(res.Restored) != 1 || read(t, dst, `Photos\a.jpg`) != "AAA" {
		t.Fatalf("restore with overwrite: %+v", res)
	}
	runs, _ = List(dst)
	if len(runs) != 1 || runs[0].Entries[0].Why != Replaced {
		t.Fatalf("the replaced NEW file must be in a new run; runs = %+v", runs)
	}
}

func TestRunWithoutRunJSONIsStillListed(t *testing.T) {
	dst := t.TempDir()
	write(t, dst, "x.txt", "x")
	r := Begin(dst, "copy", "", time.Now())
	r.Move("x.txt", Replaced) // and the process dies before Close
	runs, _ := List(dst)
	if len(runs) != 1 || runs[0].Files() != 1 || runs[0].Entries[0].Why != "unknown" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestEmptyOlderThan(t *testing.T) {
	dst := t.TempDir()
	old := time.Now().Add(-40 * 24 * time.Hour)
	for i, when := range []time.Time{old, time.Now()} {
		write(t, dst, "f.txt", "v")
		r := Begin(dst, "copy", "", when.Add(time.Duration(i)*time.Second))
		r.Move("f.txt", Replaced)
		r.Close()
	}
	gone, err := Empty(dst, 30*24*time.Hour, time.Now())
	if err != nil || len(gone) != 1 {
		t.Fatalf("Empty = %v, %v", gone, err)
	}
	if runs, _ := List(dst); len(runs) != 1 {
		t.Errorf("the recent run must stay; %d left", len(runs))
	}
	Empty(dst, 0, time.Now())
	if _, err := os.Stat(Dir(dst)); err == nil {
		t.Error("an empty trash folder should be removed")
	}
}

func TestSamePathTwiceInOneRunKeepsBoth(t *testing.T) {
	dst := t.TempDir()
	r := Begin(dst, "copy", "", time.Now())
	write(t, dst, "f.txt", "one")
	r.Move("f.txt", Replaced)
	write(t, dst, "f.txt", "two")
	r.Move("f.txt", Replaced)
	r.Close()
	if n, _ := r.Moved(); n != 2 {
		t.Fatalf("moved %d", n)
	}
	ents, _ := os.ReadDir(filepath.Join(Dir(dst), r.ID()))
	if len(ents) != 3 { // f.txt, f (2).txt, run.json
		t.Errorf("trash run holds %d entries", len(ents))
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 720 * time.Hour, "2w": 336 * time.Hour, "12h": 12 * time.Hour, "": 0} {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("ParseAge(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseAge("soon"); err == nil {
		t.Error("ParseAge(soon) must fail")
	}
}
