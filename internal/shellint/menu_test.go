package shellint

import (
	"reflect"
	"strings"
	"testing"
)

// parseCommandLine splits a Windows command line into arguments using the
// documented rules of CommandLineToArgvW (and the C runtime):
//
//   - spaces and tabs separate arguments unless inside double quotes;
//   - 2n backslashes followed by a quote -> n backslashes, and the quote is a
//     delimiter; 2n+1 backslashes followed by a quote -> n backslashes and a
//     LITERAL quote;
//   - backslashes not followed by a quote are literal.
func parseCommandLine(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
					i++
				}
				// even: the quote is handled by the next loop turn as a delimiter
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
			}
			have = true
		case c == '"':
			inQuote = !inQuote
			have = true
			i++
		case (c == ' ' || c == '\t') && !inQuote:
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
			i++
		default:
			cur.WriteByte(c)
			have = true
			i++
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

func TestCommandLineParserMatchesDocumentedBehaviour(t *testing.T) {
	cases := map[string][]string{
		`a b  c`:                 {"a", "b", "c"},
		`"a b" c`:                {"a b", "c"},
		`x "C:\My Folder\f.txt"`: {"x", `C:\My Folder\f.txt`},
		`x "E:\" --pause`:        {"x", `E:" --pause`}, // the classic trap: \" is an escaped quote
		`x "E:\\" --pause`:       {"x", `E:\`, "--pause"},
		`x "E:\ " --pause`:       {"x", `E:\ `, "--pause"},
		`a\\\\b "c\\\"d"`:        {`a\\\\b`, `c\"d`},
	}
	for in, want := range cases {
		if got := parseCommandLine(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parse(%q) = %q, want %q", in, got, want)
		}
	}
}

const exe = `C:\Program Files\DriveDex\drivedex.exe`

func substitute(cmd, placeholder, value string) string {
	return strings.ReplaceAll(cmd, placeholder, value)
}

// THE reason for the trailing space: a drive root as Explorer passes it.
func TestDriveRootsSurviveTheCommandLine(t *testing.T) {
	var searchDrive, searchBackground, searchFolder, indexDrive Entry
	for _, e := range Entries(exe) {
		switch {
		case strings.HasSuffix(e.Key, `Drive\shell\DriveDex`):
			searchDrive = e
		case strings.HasSuffix(e.Key, `Background\shell\DriveDex`):
			searchBackground = e
		case strings.HasSuffix(e.Key, `Directory\shell\DriveDex`):
			searchFolder = e
		case strings.HasSuffix(e.Key, `DriveDexIndex`):
			indexDrive = e
		}
	}
	trim := func(s string) string { return strings.TrimSpace(s) } // what DriveDex does with every path argument

	// Right-click a drive: Explorer substitutes %1 with `E:\`.
	got := parseCommandLine(substitute(searchDrive.Command, "%1", `E:\`))
	if len(got) != 4 || got[0] != exe || got[1] != "tui" || got[2] != "--scope" || trim(got[3]) != `E:\` {
		t.Errorf("search a drive: %q", got)
	}
	got = parseCommandLine(substitute(indexDrive.Command, "%1", `E:\`))
	if len(got) != 4 || got[1] != "index" || trim(got[2]) != `E:\` || got[3] != "--pause" {
		t.Errorf("index a drive must keep --pause as its own argument: %q", got)
	}
	// Right-click empty space inside a drive's root window: %V is `E:\` too.
	got = parseCommandLine(substitute(searchBackground.Command, "%V", `E:\`))
	if len(got) != 4 || trim(got[3]) != `E:\` {
		t.Errorf("background of a drive root: %q", got)
	}
	// Ordinary folders, including spaces and non-ASCII.
	for _, folder := range []string{`C:\Users\Me\My Photos`, `D:\Projects\résumés 2026`, `C:\x`} {
		got = parseCommandLine(substitute(searchFolder.Command, "%1", folder))
		if len(got) != 4 || trim(got[3]) != folder {
			t.Errorf("folder %q: %q", folder, got)
		}
	}

	// And the form BUILD.md suggested really is broken for drives (this is why we deviate).
	naive := `"` + exe + `" index "%1" --pause`
	broken := parseCommandLine(substitute(naive, "%1", `E:\`))
	if len(broken) != 3 || !strings.Contains(broken[2], `" --pause`) {
		t.Errorf("expected the naive command line to mangle `E:\\`, got %q", broken)
	}
}

func TestEntriesMatchTheSpecTable(t *testing.T) {
	es := Entries(exe)
	if len(es) != 4 {
		t.Fatalf("%d entries", len(es))
	}
	wantKeys := []string{
		`Software\Classes\Directory\shell\DriveDex`,
		`Software\Classes\Directory\Background\shell\DriveDex`,
		`Software\Classes\Drive\shell\DriveDex`,
		`Software\Classes\Drive\shell\DriveDexIndex`,
	}
	wantTitles := []string{"Search with DriveDex", "Search with DriveDex", "Search with DriveDex", "Index with DriveDex"}
	for i, e := range es {
		if e.Key != wantKeys[i] || e.Title != wantTitles[i] {
			t.Errorf("entry %d = %q / %q", i, e.Key, e.Title)
		}
		if e.Icon != `"`+exe+`",0` {
			t.Errorf("icon = %q", e.Icon)
		}
		if !strings.HasPrefix(e.Command, `"`+exe+`" `) {
			t.Errorf("the command must start with the quoted absolute path: %q", e.Command)
		}
	}
	if !strings.Contains(es[0].Command, "%1") || !strings.Contains(es[1].Command, "%V") {
		t.Error("folders use %1, folder backgrounds use %V")
	}
	if !strings.Contains(es[3].Command, "index") || !strings.HasSuffix(es[3].Command, "--pause") {
		t.Errorf("index entry = %q", es[3].Command)
	}
}

// ---- install / uninstall / status against a fake registry ---------------------------------

type fakeReg struct{ keys map[string]map[string]string }

func newFakeReg() *fakeReg { return &fakeReg{keys: map[string]map[string]string{}} }

func (f *fakeReg) SetValue(key, name, value string) error {
	if f.keys[key] == nil {
		f.keys[key] = map[string]string{}
	}
	f.keys[key][name] = value
	return nil
}
func (f *fakeReg) GetValue(key, name string) (string, bool, error) {
	v, ok := f.keys[key][name]
	return v, ok, nil
}
func (f *fakeReg) DeleteTree(key string) (bool, error) {
	removed := false
	for k := range f.keys {
		if k == key || strings.HasPrefix(k, key+`\`) {
			delete(f.keys, k)
			removed = true
		}
	}
	return removed, nil
}

func TestInstallWritesEveryKeyAndValue(t *testing.T) {
	reg := newFakeReg()
	if err := Install(reg, exe); err != nil {
		t.Fatal(err)
	}
	for _, e := range Entries(exe) {
		if reg.keys[e.Key][""] != e.Title || reg.keys[e.Key]["Icon"] != e.Icon || reg.keys[e.Key+`\command`][""] != e.Command {
			t.Errorf("%s not written correctly: %v / %v", e.Key, reg.keys[e.Key], reg.keys[e.Key+`\command`])
		}
	}
	if len(reg.keys) != 8 { // 4 entries x (key + command subkey)
		t.Errorf("wrote %d keys, want exactly 8", len(reg.keys))
	}
	for k := range reg.keys {
		if !strings.HasPrefix(k, `Software\Classes\`) {
			t.Errorf("%s is outside HKCU\\Software\\Classes", k)
		}
	}
	// Running it twice changes nothing.
	before := len(reg.keys)
	Install(reg, exe)
	if len(reg.keys) != before {
		t.Error("install must be idempotent")
	}
}

func TestUninstallRemovesExactlyOurKeys(t *testing.T) {
	reg := newFakeReg()
	// Neighbours that must survive.
	reg.SetValue(`Software\Classes\Directory\shell\SomeOtherTool`, "", "Do other thing")
	reg.SetValue(`Software\Classes\Directory\shell\SomeOtherTool\command`, "", `"C:\other.exe" "%1"`)
	reg.SetValue(`Software\Classes\Drive\shell\DriveDexExtra`, "", "not ours, similar name")
	reg.SetValue(`Software\Classes\Drive\shell\open`, "", "Open")
	Install(reg, exe)

	results, err := Uninstall(reg)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, r := range results {
		if r.Removed {
			removed++
		}
	}
	if len(results) != 4 || removed != 4 {
		t.Errorf("results = %+v", results)
	}
	want := map[string]bool{
		`Software\Classes\Directory\shell\SomeOtherTool`:         true,
		`Software\Classes\Directory\shell\SomeOtherTool\command`: true,
		`Software\Classes\Drive\shell\DriveDexExtra`:             true,
		`Software\Classes\Drive\shell\open`:                      true,
	}
	for k := range reg.keys {
		if !want[k] {
			t.Errorf("a key we should have removed is still there: %s", k)
		}
	}
	for k := range want {
		if _, ok := reg.keys[k]; !ok {
			t.Errorf("uninstall deleted something that isn't ours: %s", k)
		}
	}
	// Second time: nothing to do, no error.
	results, err = Uninstall(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Removed || r.Skipped != "" {
			t.Errorf("second uninstall: %+v", r)
		}
	}
}

func TestUninstallLeavesAForeignEntryWithTheSameNameAlone(t *testing.T) {
	reg := newFakeReg()
	key := `Software\Classes\Directory\shell\DriveDex`
	reg.SetValue(key, "", "Some Other Program Using This Name")
	reg.SetValue(key+`\command`, "", `"C:\Tools\sneaky.exe" "%1"`)
	results, _ := Uninstall(reg)
	var skipped bool
	for _, r := range results {
		if r.Key == key {
			skipped = r.Skipped != "" && !r.Removed
		}
	}
	if !skipped || reg.keys[key+`\command`][""] == "" {
		t.Errorf("a key that isn't DriveDex's must be left alone: %+v", results)
	}
}

func TestUninstallCleansUpAHalfInstalledEntry(t *testing.T) {
	reg := newFakeReg()
	key := `Software\Classes\Drive\shell\DriveDexIndex`
	reg.SetValue(key, "", "Index with DriveDex") // the command subkey never got written
	results, _ := Uninstall(reg)
	for _, r := range results {
		if r.Key == key && !r.Removed {
			t.Errorf("a leftover DriveDex key without a command should be removed: %+v", r)
		}
	}
	if len(reg.keys) != 0 {
		t.Errorf("keys left: %v", reg.keys)
	}
}

func TestStatusShowsMissingCurrentAndStale(t *testing.T) {
	reg := newFakeReg()
	sts, _ := Status(reg, exe)
	for _, s := range sts {
		if s.State != Missing {
			t.Errorf("empty registry: %+v", s)
		}
	}
	Install(reg, exe)
	sts, _ = Status(reg, exe)
	for _, s := range sts {
		if s.State != Current {
			t.Errorf("fresh install: %+v", s)
		}
	}
	// The program is moved: the entries now point at a path that no longer matches.
	moved := `D:\Tools\drivedex.exe`
	sts, _ = Status(reg, moved)
	for _, s := range sts {
		if s.State != Stale || !strings.Contains(s.Command, "Program Files") {
			t.Errorf("after moving: %+v", s)
		}
	}
	// Re-installing from the new place repairs it.
	Install(reg, moved)
	sts, _ = Status(reg, moved)
	for _, s := range sts {
		if s.State != Current {
			t.Errorf("after repair: %+v", s)
		}
	}
}
