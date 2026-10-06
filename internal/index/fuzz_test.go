package index

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzRead feeds damaged and random bytes to the .ddx reader. It must either
// load the data or return ErrCorruptIndex - never panic, hang or allocate
// absurd amounts of memory - and whatever it loads must survive a write/read
// round trip and path building (which walks parent IDs that may form cycles).
func FuzzRead(f *testing.F) {
	ix := New()
	ix.Header = Header{SourceKind: KindFolder, SourceID: "dir-1", RootPath: `C:\x`}
	ix.Add(Entry{ID: 1, Name: "Photos", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "IMG_0001.jpg", Size: 1234, ModTime: 1700000000})
	ix.Add(Entry{ID: 3, ParentID: 3, Name: "loop", Flags: FlagDir}) // its own parent
	var buf bytes.Buffer
	if err := Write(&buf, ix); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("DDX1"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Read(data)
		if err != nil {
			if !errors.Is(err, ErrCorruptIndex) {
				t.Fatalf("error does not wrap ErrCorruptIndex: %v", err)
			}
			return
		}
		for slot := 0; slot < got.Len(); slot++ {
			_ = got.RelPath(slot)
			_ = got.Under(slot, got.IDs[0])
		}
		var out bytes.Buffer
		if err := Write(&out, got); err != nil {
			t.Fatalf("re-writing a loaded index: %v", err)
		}
		again, err := Read(out.Bytes())
		if err != nil {
			t.Fatalf("an index DriveDex wrote can't be read back: %v", err)
		}
		if again.Len() != got.Len() {
			t.Fatalf("round trip changed the entry count: %d -> %d", got.Len(), again.Len())
		}
	})
}
