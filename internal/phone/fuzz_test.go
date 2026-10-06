package phone

import (
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

// FuzzParser feeds arbitrary phone output through both line parsers. The
// resulting index must be consistent: every entry's parent exists and is a
// folder (so no path starts with the orphan marker), no name is empty, ".",
// ".." or contains a separator, and paths can be built.
func FuzzParser(f *testing.F) {
	f.Add("123|1700000000|/sdcard/DCIM/Camera/IMG_1.jpg\n5|1|/sdcard/a|b.txt\n")
	f.Add("/sdcard/Download/x.pdf\n/sdcard//double//slash.txt\n")
	f.Add("1|2|/sdcard/a/../b/./c.txt\n")
	f.Add("1|2|/sdcard/dir/\n0|0|/sdcard\n")
	f.Fuzz(func(t *testing.T, out string) {
		for _, stat := range []bool{true, false} {
			b := index.NewBuilder(index.Header{})
			p := NewParser(b, "/sdcard/")
			for _, line := range strings.Split(out, "\n") {
				if stat {
					p.ParseStatLine(line)
				} else {
					p.ParseNameLine(line)
				}
			}
			ix := b.Index()
			if int64(ix.Len()) != p.Entries() {
				t.Fatalf("index has %d entries but the parser counted %d", ix.Len(), p.Entries())
			}
			for slot := 0; slot < ix.Len(); slot++ {
				name := ix.Names[slot]
				if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/") {
					t.Fatalf("bad entry name %q (line input %q)", name, out)
				}
				if par := ix.Parents[slot]; par != 0 {
					ps, ok := ix.Slot(par)
					if !ok || ix.Flags[ps]&index.FlagDir == 0 {
						t.Fatalf("entry %q has a parent that is missing or not a folder", name)
					}
				}
				_ = ix.RelPath(slot)
			}
		}
	})
}

// FuzzParseDevices: any `adb devices -l` output parses without panicking.
func FuzzParseDevices(f *testing.F) {
	f.Add("List of devices attached\nR58M1234ABC   device usb:1-1 model:Pixel_7\nX unauthorized\n")
	f.Add("* daemon started\nList of devices attached\nABC\tno permissions (user in plugdev group); see [http://developer.android.com/tools/device.html]\n")
	f.Fuzz(func(t *testing.T, s string) {
		for _, d := range ParseDevices(s) {
			if d.Serial == "" || d.State == "" {
				t.Fatalf("device with an empty serial or state: %+v", d)
			}
		}
	})
}
