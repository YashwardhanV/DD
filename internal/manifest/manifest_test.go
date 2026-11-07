package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func sample(i int) Record {
	r := Record{
		Rel:        fmt.Sprintf(`Photos\%d\IMG_%04d.jpg`, i%7, i),
		SrcID:      []string{"vol-1A2B3C4D", "adb-R58M", "dir-abc"}[i%3],
		SrcFileID:  uint64(1000 + i),
		SrcSize:    int64(i * 4096),
		SrcMTimeNs: 1_700_000_000_123_456_789 + int64(i),
		DstSize:    int64(i * 4096),
		DstMTimeNs: 1_700_000_000_123_456_700 + int64(i),
		SrcUSN:     int64(i * 100),
		JournalID:  0xABCDEF,
		CopiedAt:   1_731_000_000,
		Flags:      uint8(i % 16),
	}
	if i%2 == 0 {
		r.HashKind, r.Hash = HashXXH3, [16]byte{byte(i), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i >> 8)}
	}
	if i%5 == 0 {
		r.SrcRel = fmt.Sprintf(`Photos\%d\IMG:%04d.jpg`, i%7, i)
	}
	return r
}

func all(m *Manifest) []Record {
	var out []Record
	m.Range(func(r Record) bool { out = append(out, r); return true })
	sort.Slice(out, func(a, b int) bool { return out[a].Rel < out[b].Rel })
	return out
}

func TestManifestRoundTrip(t *testing.T) {
	m := New()
	for i := 0; i < 300; i++ {
		m.Put(sample(i))
	}
	m.Put(Record{Rel: "top.txt", SrcID: "vol-1"}) // a file in the root folder
	var buf bytes.Buffer
	if err := Encode(&buf, m, "test"); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all(got), all(m)) {
		t.Fatal("records differ after a round trip")
	}
	if got.Header.RecordCount != 301 || got.Header.DirCount != 8 || got.Header.WrittenBy != "test" {
		t.Errorf("header = %+v", got.Header)
	}
	// Any change to the bytes is caught.
	for _, off := range []int{0, 5, len(buf.Bytes()) / 2, len(buf.Bytes()) - 1} {
		bad := append([]byte(nil), buf.Bytes()...)
		bad[off] ^= 0x40
		if _, err := Decode(bytes.NewReader(bad)); err == nil {
			t.Errorf("a flipped byte at %d was not detected", off)
		}
	}
}

func TestUnderAndKeys(t *testing.T) {
	m := New()
	for _, rel := range []string{`A\x.txt`, `A\B\y.txt`, `AB\z.txt`, "a.txt"} {
		m.Put(Record{Rel: rel})
	}
	if n := m.CountUnder("A"); n != 2 {
		t.Errorf("CountUnder(A) = %d, want 2 (AB is a different folder)", n)
	}
	if n := len(m.Under("")); n != 4 {
		t.Errorf("Under(\"\") = %d", n)
	}
	if _, ok := m.Get("A/B/y.txt"); !ok {
		t.Error("forward slashes must find the record")
	}
}

func TestJournalReplayStopsAtTornTail(t *testing.T) {
	var data []byte
	for i := 0; i < 5; i++ {
		data = append(data, frame(putPayload(sample(i)))...)
	}
	data = append(data, frame(deletePayload(sample(1).Rel))...)
	last := frame(putPayload(sample(99)))
	full := append(append([]byte(nil), data...), last...)

	m := New()
	if n, torn := ReplayJournal(full, m); n != 7 || torn || m.Len() != 5 {
		t.Fatalf("complete journal: applied %d, torn %v, %d records", n, torn, m.Len())
	}
	// Truncate inside the last frame at every byte offset: the first six
	// frames always survive, the last never half-applies.
	for cut := 1; cut < len(last); cut++ {
		m := New()
		n, torn := ReplayJournal(full[:len(data)+cut], m)
		if n != 6 || !torn || m.Len() != 4 {
			t.Fatalf("cut at %d: applied %d, torn %v, %d records", cut, n, torn, m.Len())
		}
		if _, ok := m.Get(sample(99).Rel); ok {
			t.Fatalf("cut at %d: the torn record was applied", cut)
		}
	}
}

func TestStoreRecoversAfterACrash(t *testing.T) {
	dst := t.TempDir()
	s, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := s.Put(sample(i)); err != nil {
			t.Fatal(err)
		}
	}
	s.Abandon() // the process "dies" before compacting

	// Tear the end of the journal, as an unplug would.
	jp := filepath.Join(Dir(dst), JournalFile)
	data, _ := os.ReadFile(jp)
	os.WriteFile(jp, data[:len(data)-3], 0o644)

	s, err = Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if n, torn := s.Recovered(); n != 9 || !torn {
		t.Errorf("recovered %d, torn %v; want 9, true", n, torn)
	}
	if s.Manifest().Len() != 9 {
		t.Errorf("%d records after recovery", s.Manifest().Len())
	}
	if _, err := os.Stat(jp); err == nil {
		t.Error("the recovered journal should have been compacted away")
	}
	s.Delete(sample(0).Rel)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := Read(dst)
	if err != nil || m.Len() != 8 {
		t.Fatalf("after close: %v, %d records", err, m.Len())
	}
}

func TestStoreIsExclusive(t *testing.T) {
	dst := t.TempDir()
	s, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dst); !errors.Is(err, ErrLocked) {
		t.Errorf("second Open = %v, want ErrLocked", err)
	}
	s.Close()
	s2, err := Open(dst)
	if err != nil {
		t.Fatalf("after Close the lock must be free: %v", err)
	}
	s2.Close()
}

func TestNoChangesLeavesNoFiles(t *testing.T) {
	dst := t.TempDir()
	s, _ := Open(dst)
	s.Close()
	if Exists(dst) {
		t.Error("a run that recorded nothing should not create a manifest")
	}
}

func FuzzDecode(f *testing.F) {
	m := New()
	for i := 0; i < 5; i++ {
		m.Put(sample(i))
	}
	var buf bytes.Buffer
	Encode(&buf, m, "fuzz")
	f.Add(buf.Bytes())
	f.Add([]byte("DDM1\x02\x00\x00\x00{}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Whatever decodes must encode and decode to the same records.
		var out bytes.Buffer
		if err := Encode(&out, m, "fuzz"); err != nil {
			t.Fatal(err)
		}
		m2, err := Decode(&out)
		if err != nil || !reflect.DeepEqual(all(m), all(m2)) {
			t.Fatalf("re-encode differs: %v", err)
		}
	})
}

func FuzzReplayJournal(f *testing.F) {
	f.Add(append(frame(putPayload(sample(1))), frame(deletePayload("x"))...))
	f.Fuzz(func(t *testing.T, data []byte) {
		ReplayJournal(data, New()) // must never panic
	})
}
