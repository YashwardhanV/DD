package winname

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFix(t *testing.T) {
	cases := map[string]string{
		"photo.jpg":         "photo.jpg",
		"notes:1.txt":       "notes：1.txt",
		"what?.jpg":         "what？.jpg",
		`a<b>c|d"e*f\g.txt`: "a＜b＞c｜d＂e＊f＼g.txt",
		"trailing.":         "trailing_",
		"trailing. ":        "trailing__",
		"CON.txt":           "CON_.txt",
		"con":               "con_",
		"Lpt1.tar.gz":       "Lpt1_.tar.gz",
		"COM²":              "COM²_",
		"COM0.txt":          "COM0.txt", // only 1-9 are reserved
		"CONSOLE.txt":       "CONSOLE.txt",
		"tab\there":         "tab�here",
		"..":                "__",
		"":                  "_",
	}
	for in, want := range cases {
		got, changed := Fix(in)
		if got != want {
			t.Errorf("Fix(%q) = %q, want %q", in, got, want)
		}
		if changed != (in != want) {
			t.Errorf("Fix(%q) changed = %v", in, changed)
		}
		if !Valid(got) {
			t.Errorf("Fix(%q) = %q is still not valid (%v)", in, got, Check(got))
		}
	}
}

func TestLongNamesKeepTheirExtensionAndStayDistinct(t *testing.T) {
	a := strings.Repeat("a", 300) + ".jpg"
	b := strings.Repeat("a", 299) + "b.jpg"
	fa, _ := Fix(a)
	fb, _ := Fix(b)
	if Units(fa) > MaxUnits || !strings.HasSuffix(fa, ".jpg") || !strings.Contains(fa, "~") {
		t.Errorf("Fix(long) = %q (%d units)", fa, Units(fa))
	}
	if fa == fb {
		t.Error("two different long names must not become the same name")
	}
	// Characters outside the BMP take two UTF-16 units each.
	emoji := strings.Repeat("😀", 200) + ".png"
	fe, _ := Fix(emoji)
	if Units(fe) > MaxUnits || !utf8.ValidString(fe) {
		t.Errorf("emoji name: %d units, valid UTF-8 %v", Units(fe), utf8.ValidString(fe))
	}
}

func TestCaseVariant(t *testing.T) {
	if got := CaseVariant("photo.jpg", 2); got != "photo (case 2).jpg" {
		t.Errorf("got %q", got)
	}
	if got := CaseVariant("README", 3); got != "README (case 3)" {
		t.Errorf("got %q", got)
	}
}

// FuzzFix: Fix always returns a valid name, never changes a valid one, and is
// stable (fixing a fixed name changes nothing).
func FuzzFix(f *testing.F) {
	for _, s := range []string{"a", "CON", "a:b", "x.", strings.Repeat("é", 300), "\x00", "\xff\xfe", "COM¹.txt", " "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, changed := Fix(s)
		if !Valid(got) {
			t.Fatalf("Fix(%q) = %q is invalid: %v", s, got, Check(got))
		}
		if Valid(s) && (changed || got != s) {
			t.Fatalf("valid name %q was changed to %q", s, got)
		}
		if again, ch := Fix(got); ch || again != got {
			t.Fatalf("Fix is not stable: %q -> %q -> %q", s, got, again)
		}
	})
}
