package index

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"reflect"
	"testing"
	"time"
)

// sample returns an index with awkward content: Unicode, empty names' parents,
// unknown sizes, negative values and every flag.
func sample() *Index {
	ix := New()
	ix.Header = Header{
		SourceKind: KindFolder, SourceID: "dir-abc123def456", Label: "Test",
		RootPath: `D:\Test`, Scanner: "walk",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC),
		ScanMillis: 1234, UsnJournalID: 77, NextUsn: 99,
	}
	ix.Add(Entry{ID: 1, ParentID: 0, Name: "Photos", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "résumé.pdf", Size: 212 * 1024, ModTime: 1700000000})
	ix.Add(Entry{ID: 3, ParentID: 1, Name: "फोटो.jpg", Size: 0, ModTime: 1})
	ix.Add(Entry{ID: 1 << 60, ParentID: 1, Name: "写真.png", Size: -1, Flags: FlagNoMeta | FlagHidden | FlagSystem})
	ix.Add(Entry{ID: 5, ParentID: 0, Name: "with space & |pipe|.txt", Size: 1<<40 + 5, ModTime: -5})
	return ix
}

func entriesOf(ix *Index) []Entry {
	out := make([]Entry, ix.Len())
	for i := range out {
		out[i] = ix.Entry(i)
	}
	return out
}

func TestFormatRoundTrip(t *testing.T) {
	orig := sample()
	var buf bytes.Buffer
	if err := Write(&buf, orig); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(buf.Bytes())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(entriesOf(got), entriesOf(orig)) {
		t.Errorf("entries differ after round trip:\n got %+v\nwant %+v", entriesOf(got), entriesOf(orig))
	}
	wantHeader := orig.Header
	wantHeader.FormatVersion, wantHeader.EntryCount = FormatVersion, 5
	if !got.Header.CreatedAt.Equal(wantHeader.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.Header.CreatedAt, wantHeader.CreatedAt)
	}
	got.Header.CreatedAt, got.Header.UpdatedAt = wantHeader.CreatedAt, wantHeader.UpdatedAt
	if !reflect.DeepEqual(got.Header, wantHeader) {
		t.Errorf("header differs:\n got %+v\nwant %+v", got.Header, wantHeader)
	}
	// Derived data is rebuilt on load.
	if got.Lower[1] != "résumé.pdf" {
		t.Errorf("Lower not rebuilt: %q", got.Lower[1])
	}
	if got.RelPath(1) != `Photos\résumé.pdf` {
		t.Errorf("RelPath after load = %q", got.RelPath(1))
	}
}

func TestWriteSkipsTombstones(t *testing.T) {
	ix := sample()
	ix.Delete(3)
	var buf bytes.Buffer
	if err := Write(&buf, ix); err != nil {
		t.Fatal(err)
	}
	got, err := Read(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 4 || got.Header.EntryCount != 4 {
		t.Errorf("Len=%d EntryCount=%d, want 4/4", got.Len(), got.Header.EntryCount)
	}
	if _, ok := got.Slot(3); ok {
		t.Error("deleted entry was written to disk")
	}
	for _, f := range got.Flags {
		if f&FlagDeleted != 0 {
			t.Error("FlagDeleted must never reach the file")
		}
	}
}

func TestEmptyIndexRoundTrip(t *testing.T) {
	ix := New()
	ix.Header.SourceID = "dir-000000000000"
	var buf bytes.Buffer
	if err := Write(&buf, ix); err != nil {
		t.Fatal(err)
	}
	got, err := Read(buf.Bytes())
	if err != nil || got.Len() != 0 {
		t.Fatalf("empty index: len=%v err=%v", got, err)
	}
}

func TestEveryFlippedByteIsDetected(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	for i := range data {
		bad := append([]byte(nil), data...)
		bad[i] ^= 0x01
		if _, err := Read(bad); !errors.Is(err, ErrCorruptIndex) {
			t.Fatalf("flipping byte %d: err = %v, want ErrCorruptIndex", i, err)
		}
	}
}

func TestTruncatedFileGivesErrorNotPanic(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	for n := 0; n < len(data); n++ {
		if _, err := Read(data[:n]); !errors.Is(err, ErrCorruptIndex) {
			t.Fatalf("truncated to %d bytes: err = %v, want ErrCorruptIndex", n, err)
		}
	}
}

func TestGarbageGivesErrorNotPanic(t *testing.T) {
	for _, data := range [][]byte{nil, {}, []byte("hello"), bytes.Repeat([]byte{0xff}, 100), []byte("DDX1\x01\x00\xff\xff\xff\xff")} {
		if _, err := Read(data); !errors.Is(err, ErrCorruptIndex) {
			t.Errorf("Read(%q) err = %v, want ErrCorruptIndex", data, err)
		}
	}
}

// writeV1 encodes ix in the version 1 layout (no nanos field), exactly as
// DriveDex 1.x wrote it.
func writeV1(ix *Index) []byte {
	var body bytes.Buffer
	var tmp [binary.MaxVarintLen64]byte
	h := ix.Header
	h.FormatVersion = 1
	hj, _ := json.Marshal(h)
	body.WriteString(magic)
	binary.LittleEndian.PutUint16(tmp[:2], 1)
	body.Write(tmp[:2])
	binary.LittleEndian.PutUint32(tmp[:4], uint32(len(hj)))
	body.Write(tmp[:4])
	body.Write(hj)
	body.Write(tmp[:binary.PutUvarint(tmp[:], uint64(ix.Len()))])
	for s := 0; s < ix.Len(); s++ {
		body.Write(tmp[:binary.PutUvarint(tmp[:], ix.IDs[s])])
		body.Write(tmp[:binary.PutUvarint(tmp[:], ix.Parents[s])])
		body.WriteByte(ix.Flags[s])
		body.Write(tmp[:binary.PutVarint(tmp[:], ix.Sizes[s])])
		body.Write(tmp[:binary.PutVarint(tmp[:], ix.MTimes[s])])
		body.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(ix.Names[s])))])
		body.WriteString(ix.Names[s])
	}
	binary.LittleEndian.PutUint32(tmp[:4], crc32.ChecksumIEEE(body.Bytes()))
	body.Write(tmp[:4])
	return body.Bytes()
}

// TestINV8_Version1IndexStillLoads: an index written by DriveDex 1.x loads in
// this version, with whole-second times (not "exact").
func TestINV8_Version1IndexStillLoads(t *testing.T) {
	orig := sample()
	orig.Header.MTimeUnit = "ns" // even if a v1 header somehow claimed it
	got, err := Read(writeV1(orig))
	if err != nil {
		t.Fatalf("version 1 file: %v", err)
	}
	if !reflect.DeepEqual(entriesOf(got), entriesOf(orig)) {
		t.Errorf("entries differ:\n got %+v\nwant %+v", entriesOf(got), entriesOf(orig))
	}
	if got.Header.ExactTimes() {
		t.Error("a version 1 index only has whole seconds")
	}
}

func TestNanosecondTimesRoundTrip(t *testing.T) {
	ix := New()
	ix.Header = Header{SourceID: "dir-x", MTimeUnit: "ns"}
	ix.Add(Entry{ID: 1, Name: "a", Size: 3, ModTime: 1_700_000_000, Nanos: 123_456_700})
	ix.Add(Entry{ID: 2, Name: "b", Size: 3, ModTime: -5, Nanos: 999_999_999})
	var buf bytes.Buffer
	if err := Write(&buf, ix); err != nil {
		t.Fatal(err)
	}
	got, err := Read(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.ModTimeNs(0) != 1_700_000_000*1e9+123_456_700 || got.Nanos[1] != 999_999_999 || !got.Header.ExactTimes() {
		t.Errorf("times after round trip: %d %d exact=%v", got.ModTimeNs(0), got.Nanos[1], got.Header.ExactTimes())
	}
}
