package ntfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func TestParseRecordsThreeCraftedRecords(t *testing.T) {
	want := []Record{
		{FRN: 0x0001000000000040, ParentFRN: 0x0005000000000005, USN: 4160, Reason: 0x100, Attrs: AttrDirectory, Name: "Photos"},
		{FRN: 0x0001000000000041, ParentFRN: 0x0001000000000040, USN: 4224, Reason: 0, Attrs: AttrHidden | AttrSystem, Name: "résumé final.pdf"},
		// CJK plus an emoji (a UTF-16 surrogate pair) - the tricky part of UTF-16 decoding.
		{FRN: 0x0003000000000042, ParentFRN: 0x0001000000000040, USN: 1 << 40, Reason: UsnReasonFileDelete | 0x80000000, Attrs: 0x20, Name: "写真 📷.png"},
	}
	var buf []byte
	for _, r := range want {
		buf = append(buf, EncodeRecord(r)...)
	}
	got, err := ParseRecords(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if !got[0].IsDir() || got[1].IsDir() || !got[2].IsDelete() || got[0].IsDelete() {
		t.Error("IsDir / IsDelete wrong")
	}
}

func TestEncodeRecordUsesTheDocumentedLayout(t *testing.T) {
	b := EncodeRecord(Record{FRN: 7, ParentFRN: 5, USN: 9, Reason: 0x200, Attrs: 0x10, Name: "ab"})
	le := binary.LittleEndian
	switch {
	case int(le.Uint32(b[0:])) != len(b) || len(b)%8 != 0:
		t.Errorf("RecordLength %d vs %d bytes (must be 8-aligned)", le.Uint32(b[0:]), len(b))
	case le.Uint16(b[4:]) != 2:
		t.Error("MajorVersion must be 2")
	case le.Uint64(b[8:]) != 7 || le.Uint64(b[16:]) != 5 || le.Uint64(b[24:]) != 9:
		t.Error("FRN / parent / USN offsets wrong")
	case le.Uint32(b[40:]) != 0x200 || le.Uint32(b[52:]) != 0x10:
		t.Error("Reason / FileAttributes offsets wrong")
	case le.Uint16(b[56:]) != 4 || le.Uint16(b[58:]) != 60:
		t.Error("FileNameLength must be in BYTES (4) and FileNameOffset 60")
	case !bytes.Equal(b[60:64], []byte{'a', 0, 'b', 0}):
		t.Errorf("name bytes = %v", b[60:64])
	}
}

func TestParseRecordsSkipsOtherVersions(t *testing.T) {
	v3 := make([]byte, 80)
	binary.LittleEndian.PutUint32(v3[0:], 80)
	binary.LittleEndian.PutUint16(v3[4:], 3) // a V3 record
	buf := append(append(EncodeRecord(Record{FRN: 1, Name: "a"}), v3...), EncodeRecord(Record{FRN: 2, Name: "b"})...)
	got, err := ParseRecords(buf)
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestParseRecordsEmptyBuffer(t *testing.T) {
	if got, err := ParseRecords(nil); err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// Every possible truncation of valid data, and corrupted lengths, must give an
// error or a clean result - never a panic or an endless loop.
func TestParseRecordsTruncatedAndCorruptNeverPanics(t *testing.T) {
	var buf []byte
	for _, name := range []string{"one", "twö", "写真.png"} {
		buf = append(buf, EncodeRecord(Record{FRN: 9, ParentFRN: 5, Name: name})...)
	}
	for n := 1; n < len(buf); n++ {
		_, err := ParseRecords(buf[:n])
		if err == nil && n != len(EncodeRecord(Record{FRN: 9, ParentFRN: 5, Name: "one"})) &&
			n != len(EncodeRecord(Record{FRN: 9, Name: "one"}))+len(EncodeRecord(Record{FRN: 9, Name: "twö"})) {
			// A prefix that ends exactly on a record boundary is legitimately fine.
			t.Fatalf("truncating to %d bytes gave no error", n)
		}
		if err != nil && !errors.Is(err, ErrBadRecord) {
			t.Fatalf("truncating to %d bytes: error %v isn't ErrBadRecord", n, err)
		}
	}

	corrupt := func(mutate func(b []byte)) error {
		b := append([]byte(nil), EncodeRecord(Record{FRN: 9, Name: "name"})...)
		mutate(b)
		_, err := ParseRecords(b)
		return err
	}
	cases := map[string]func(b []byte){
		"zero length (would loop forever)": func(b []byte) { binary.LittleEndian.PutUint32(b[0:], 0) },
		"length beyond buffer":             func(b []byte) { binary.LittleEndian.PutUint32(b[0:], 9999) },
		"V2 record too short":              func(b []byte) { binary.LittleEndian.PutUint32(b[0:], 16) },
		"name runs past record":            func(b []byte) { binary.LittleEndian.PutUint16(b[56:], 200) },
		"odd name length":                  func(b []byte) { binary.LittleEndian.PutUint16(b[56:], 3) },
		"name offset inside header":        func(b []byte) { binary.LittleEndian.PutUint16(b[58:], 10) },
	}
	for name, mutate := range cases {
		if err := corrupt(mutate); !errors.Is(err, ErrBadRecord) {
			t.Errorf("%s: err = %v, want ErrBadRecord", name, err)
		}
	}
}

func TestDecodeUTF16LE(t *testing.T) {
	u16 := func(units ...uint16) []byte {
		b := make([]byte, 2*len(units))
		for i, u := range units {
			binary.LittleEndian.PutUint16(b[2*i:], u)
		}
		return b
	}
	cases := []struct {
		in   []byte
		want string
	}{
		{nil, ""},
		{u16('h', 'i'), "hi"},              // ASCII fast path
		{u16('c', 'a', 'f', 0xE9), "café"}, // one non-ASCII unit
		{u16(0x5199, 0x771F), "写真"},        // CJK
		{u16(0xD83D, 0xDCF7), "📷"},         // surrogate pair
		{u16('a', 0x0100), "aĀ"},           // high byte set on a low-ASCII unit
		{u16(0xD83D), "�"},                 // lone surrogate becomes the replacement character
		{[]byte{'a', 0, 'b'}, "a"},         // stray odd byte is ignored
	}
	for _, c := range cases {
		if got := decodeUTF16LE(c.in); got != c.want {
			t.Errorf("decodeUTF16LE(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJournalDataRoundTrip(t *testing.T) {
	want := JournalData{ID: 0xAABBCCDDEEFF0011, FirstUsn: 100, NextUsn: 5000, LowestValidUsn: 200, MaxUsn: 1 << 62, MaximumSize: 32 << 20, AllocationDelta: 4 << 20}
	b := EncodeJournalData(want)
	if len(b) != 56 {
		t.Fatalf("USN_JOURNAL_DATA_V0 is 56 bytes, got %d", len(b))
	}
	// Real calls give us an 80-byte buffer with the first 56 filled; extra bytes are ignored.
	got, err := ParseJournalData(append(b, make([]byte, 24)...))
	if err != nil || got != want {
		t.Errorf("got %+v, %v", got, err)
	}
	if _, err := ParseJournalData(b[:55]); !errors.Is(err, ErrBadRecord) {
		t.Errorf("short data: %v", err)
	}
}

func TestRequestStructuresHaveTheDocumentedSizes(t *testing.T) {
	enum := EnumRequest{StartFRN: 1, LowUsn: 2, HighUsn: 3}
	if b := enum.Encode(); len(b) != 24 {
		t.Errorf("MFT_ENUM_DATA_V0 is 24 bytes, got %d", len(b))
	} else if back, err := DecodeEnumRequest(b); err != nil || back != enum {
		t.Errorf("enum round trip: %+v %v", back, err)
	}
	read := ReadRequest{StartUsn: 1, ReasonMask: 0xFFFFFFFF, ReturnOnlyOnClose: 0, Timeout: 0, BytesToWaitFor: 0, JournalID: 99}
	if b := read.Encode(); len(b) != 40 {
		t.Errorf("READ_USN_JOURNAL_DATA_V0 is 40 bytes, got %d", len(b))
	} else if back, err := DecodeReadRequest(b); err != nil || back != read {
		t.Errorf("read round trip: %+v %v", back, err)
	}
	if b := EncodeCreateJournal(32<<20, 4<<20); len(b) != 16 || binary.LittleEndian.Uint64(b) != 32<<20 || binary.LittleEndian.Uint64(b[8:]) != 4<<20 {
		t.Errorf("CREATE_USN_JOURNAL_DATA = %v", b)
	}
	if _, err := DecodeEnumRequest(make([]byte, 23)); err == nil {
		t.Error("short enum request should fail")
	}
	if _, err := DecodeReadRequest(make([]byte, 39)); err == nil {
		t.Error("short read request should fail")
	}
}

func TestRecordNumberStripsSequence(t *testing.T) {
	if got := RecordNumber(0x0005000000000005); got != 5 {
		t.Errorf("got %d", got)
	}
	if got := RecordNumber(0xFFFF00000000ABCD); got != 0xABCD {
		t.Errorf("got %#x", got)
	}
}
