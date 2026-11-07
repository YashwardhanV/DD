package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/preflight"
)

// runIn2 is run with an answer on stdin, keeping stdout and stderr apart.
func runIn2(t *testing.T, in string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd("test")
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(in))
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// deepTree makes a source whose files' paths are longer than Explorer can open
// once copied (a warning DriveDex can't fix by itself). The warning is
// turned on even off Windows, where Explorer's limit doesn't apply.
func deepTree(t *testing.T) string {
	t.Helper()
	old := preflight.CheckLongPaths
	preflight.CheckLongPaths = true
	t.Cleanup(func() { preflight.CheckLongPaths = old })
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	src := t.TempDir()
	deep := filepath.Join(src, strings.Repeat("d", 120), strings.Repeat("e", 120))
	os.MkdirAll(deep, 0o755)
	os.WriteFile(filepath.Join(deep, "file.txt"), []byte("x"), 0o644)
	return src
}

func TestCopyAsksBeforeContinuingPastAWarning(t *testing.T) {
	src := deepTree(t)
	dst := t.TempDir()
	out, _, err := runIn2(t, "n\n", "copy", src, dst)
	var ue *UserError
	if !errors.As(err, &ue) || ue.Code != exitBlocked {
		t.Fatalf("answering n: err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Continue? [y/N]") || !strings.Contains(out, "longer than 259 characters") {
		t.Errorf("output:\n%s", out)
	}
	if n := countFilesUnder(t, dst); n != 0 {
		t.Errorf("%d files copied after answering no", n)
	}
	if _, _, err := runIn2(t, "y\n", "copy", src, dst); err != nil {
		t.Fatalf("answering y: %v", err)
	}
	if n := countFilesUnder(t, dst); n != 1 {
		t.Errorf("%d files copied after answering yes, want 1", n)
	}
}

func TestCopyYesSkipsTheQuestion(t *testing.T) {
	src := deepTree(t)
	out, _, err := runIn2(t, "", "copy", src, t.TempDir(), "--yes")
	if err != nil || strings.Contains(out, "Continue?") {
		t.Errorf("--yes: %v\n%s", err, out)
	}
}

func TestBlockedCopyExitsWithCode3(t *testing.T) {
	tree, _ := withFixture(t)
	fakeChecks(t, 1000, "NTFS")
	root := NewRootCmd("test")
	root.SetArgs([]string{"copy", tree, t.TempDir()})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	if code := executeRoot(context.Background(), root, strings.NewReader(""), &out); code != exitBlocked {
		t.Errorf("exit code = %d, want %d", code, exitBlocked)
	}
}

func TestCopyJSONResult(t *testing.T) {
	tree, res := withFixture(t)
	dst := t.TempDir()
	stdout, stderr, err := runIn2(t, "", "copy", tree, dst, "--json", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var js copyJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &js); err != nil {
		t.Fatalf("stdout is not one JSON object (%v):\n%s", err, stdout)
	}
	if !js.OK || js.Written != int64(res.Files) || js.Kind != "copy" || js.Preflight == nil || js.PlannedFrom != "scan" {
		t.Errorf("result %+v", js)
	}
	if !strings.Contains(stderr, "Copied ") {
		t.Errorf("the human messages belong on stderr:\n%s", stderr)
	}
	// Blocked: still one JSON object, with the exit code.
	fakeChecks(t, 1000, "NTFS")
	stdout, _, err = runIn2(t, "", "copy", tree, t.TempDir(), "--json")
	js = copyJSON{}
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &js); jerr != nil || js.OK || js.ExitCode != exitBlocked || err == nil {
		t.Errorf("blocked JSON %+v (%v), err %v", js, jerr, err)
	}
}

func TestCopyRejectsUnknownNameChoices(t *testing.T) {
	if _, err := run(t, "copy", t.TempDir(), t.TempDir(), "--on-case-collision", "merge"); err == nil || !strings.Contains(err.Error(), "--on-case-collision") {
		t.Errorf("err = %v", err)
	}
	if _, err := run(t, "copy", t.TempDir(), t.TempDir(), "--on-bad-name", "drop"); err == nil || !strings.Contains(err.Error(), "--on-bad-name") {
		t.Errorf("err = %v", err)
	}
}

func TestCopyPrintsAnEstimate(t *testing.T) {
	tree, _ := withFixture(t)
	out, err := run(t, "copy", tree, t.TempDir(), "--dry-run")
	if err != nil || !strings.Contains(out, "About ") {
		t.Errorf("%v\n%s", err, out)
	}
}
