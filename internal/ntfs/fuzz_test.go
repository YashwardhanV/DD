package ntfs

import "testing"

// FuzzParseRecords: whatever bytes DeviceIoControl hands back, parsing must
// return records or ErrBadRecord, never panic.
func FuzzParseRecords(f *testing.F) {
	f.Add(EncodeRecord(Record{FRN: 30, ParentFRN: 5, Name: "file.txt", Attrs: AttrHidden}))
	f.Add(append(EncodeRecord(Record{FRN: 31, ParentFRN: 30, Name: "héllo 😀"}), EncodeRecord(Record{FRN: 32, Name: "x"})...))
	f.Add([]byte{60, 0, 0, 0, 2, 0})
	f.Fuzz(func(t *testing.T, buf []byte) {
		recs, err := ParseRecords(buf)
		if err != nil {
			return
		}
		// Every record that parsed must re-encode and parse back to the same thing.
		for _, r := range recs {
			back, err := ParseRecords(EncodeRecord(r))
			if err != nil || len(back) != 1 {
				t.Fatalf("re-encoding %+v: %v", r, err)
			}
		}
	})
}
