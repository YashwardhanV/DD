package index

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// The .ddx file format (version 1, little-endian):
//
//	"DDX1"                     magic, 4 bytes
//	uint16                     format version (1)
//	uint32                     H = length of the header JSON
//	H bytes                    header JSON (UTF-8)
//	uvarint                    N = number of entries
//	N x entry:
//	    uvarint id
//	    uvarint parent_id
//	    byte    flags          (never includes FlagDeleted)
//	    varint  size           (signed)
//	    varint  mtime          (signed)
//	    uvarint name_len
//	    name bytes (UTF-8)
//	uint32                     CRC-32 (IEEE) of every preceding byte
//
// "varint" means the compact variable-length integers from encoding/binary:
// small numbers take 1 byte, so the file stays small.
const (
	magic         = "DDX1"
	FormatVersion = 1

	// minEntryBytes is the smallest an entry can be (id, parent, flags, size,
	// mtime, name_len each take at least one byte). It lets Read reject an
	// absurd entry count before allocating memory for it.
	minEntryBytes = 6
)

// ErrCorruptIndex is returned when an index file is damaged, truncated or not
// a DriveDex index at all. The fix is always to re-index.
var ErrCorruptIndex = errors.New("index file is corrupt")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorruptIndex, fmt.Sprintf(format, args...))
}

// Write serializes ix to w. Tombstoned entries are skipped, and the header's
// FormatVersion and EntryCount are filled in automatically.
func Write(w io.Writer, ix *Index) error {
	h := ix.Header
	h.FormatVersion = FormatVersion
	live := ix.LiveLen()
	h.EntryCount = live
	hj, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encoding index header: %w", err)
	}

	// Everything we write goes both to w and into the CRC calculation.
	crc := crc32.NewIEEE()
	bw := bufio.NewWriterSize(io.MultiWriter(w, crc), 1<<20)

	var tmp [binary.MaxVarintLen64]byte
	putUvarint := func(v uint64) { bw.Write(tmp[:binary.PutUvarint(tmp[:], v)]) }
	putVarint := func(v int64) { bw.Write(tmp[:binary.PutVarint(tmp[:], v)]) }

	bw.WriteString(magic)
	binary.LittleEndian.PutUint16(tmp[:2], FormatVersion)
	bw.Write(tmp[:2])
	binary.LittleEndian.PutUint32(tmp[:4], uint32(len(hj)))
	bw.Write(tmp[:4])
	bw.Write(hj)
	putUvarint(uint64(live))

	for slot := range ix.IDs {
		fl := ix.Flags[slot]
		if fl&FlagDeleted != 0 {
			continue
		}
		putUvarint(ix.IDs[slot])
		putUvarint(ix.Parents[slot])
		bw.WriteByte(fl)
		putVarint(ix.Sizes[slot])
		putVarint(ix.MTimes[slot])
		putUvarint(uint64(len(ix.Names[slot])))
		bw.WriteString(ix.Names[slot])
	}
	// bufio keeps the first write error and returns it here.
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}

	// The checksum is written to w only (it can't be part of its own input).
	binary.LittleEndian.PutUint32(tmp[:4], crc.Sum32())
	if _, err := w.Write(tmp[:4]); err != nil {
		return fmt.Errorf("writing index checksum: %w", err)
	}
	return nil
}

// Read parses a complete .ddx file held in memory. Any damage yields an error
// wrapping ErrCorruptIndex - never a panic.
func Read(data []byte) (*Index, error) {
	const fixed = 4 + 2 + 4 // magic, version, header length
	if len(data) < fixed+1+4 {
		return nil, corrupt("file too short (%d bytes)", len(data))
	}
	if string(data[:4]) != magic {
		return nil, corrupt("not a DriveDex index (bad magic)")
	}
	if v := binary.LittleEndian.Uint16(data[4:6]); v != FormatVersion {
		return nil, corrupt("unsupported format version %d", v)
	}
	body := data[:len(data)-4]
	if want, got := binary.LittleEndian.Uint32(data[len(data)-4:]), crc32.ChecksumIEEE(body); want != got {
		return nil, corrupt("checksum mismatch")
	}

	hlen := uint64(binary.LittleEndian.Uint32(data[6:10]))
	if hlen > uint64(len(body)-fixed) {
		return nil, corrupt("header length %d runs past the end of the file", hlen)
	}
	var h Header
	if err := json.Unmarshal(body[fixed:fixed+int(hlen)], &h); err != nil {
		return nil, corrupt("unreadable header: %v", err)
	}

	r := reader{b: body, off: fixed + int(hlen)}
	n := r.uvarint()
	if r.err == nil && n > uint64(len(body)-r.off)/minEntryBytes {
		return nil, corrupt("entry count %d is larger than the file can hold", n)
	}

	ix := &Index{
		IDs:     make([]uint64, n),
		Parents: make([]uint64, n),
		Names:   make([]string, n),
		Sizes:   make([]int64, n),
		MTimes:  make([]int64, n),
		Flags:   make([]uint8, n),
	}
	// One string copy of the whole file; every name is a slice of it. That is
	// one allocation instead of a million, and it is entirely safe code.
	all := string(body)
	for i := uint64(0); i < n && r.err == nil; i++ {
		ix.IDs[i] = r.uvarint()
		ix.Parents[i] = r.uvarint()
		ix.Flags[i] = r.byte() &^ FlagDeleted
		ix.Sizes[i] = r.varint()
		ix.MTimes[i] = r.varint()
		nameLen := r.uvarint()
		if r.err == nil && nameLen > uint64(len(body)-r.off) {
			r.err = errTruncated
		}
		if r.err == nil {
			ix.Names[i] = all[r.off : r.off+int(nameLen)]
			r.off += int(nameLen)
		}
	}
	if r.err != nil {
		return nil, corrupt("%v", r.err)
	}
	if r.off != len(body) {
		return nil, corrupt("%d unexpected bytes after the last entry", len(body)-r.off)
	}

	h.EntryCount = int(n)
	ix.Header = h
	ix.fillLower()
	ix.rebuildByID()
	return ix, nil
}

// ReadFile loads and parses one .ddx file.
func ReadFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading index %s: %w", path, err)
	}
	ix, err := Read(data)
	if err != nil {
		return nil, fmt.Errorf("loading index %s: %w", path, err)
	}
	return ix, nil
}

// ReadHeaderFile reads only the header of a .ddx file (a few hundred bytes),
// which is how `drivedex list` shows many large indexes instantly. It does not
// verify the checksum.
func ReadHeaderFile(path string) (Header, error) {
	var h Header
	f, err := os.Open(path)
	if err != nil {
		return h, fmt.Errorf("opening index %s: %w", path, err)
	}
	defer f.Close()

	var fixed [10]byte
	if _, err := io.ReadFull(f, fixed[:]); err != nil {
		return h, fmt.Errorf("%s: %w", path, corrupt("file too short"))
	}
	if string(fixed[:4]) != magic {
		return h, fmt.Errorf("%s: %w", path, corrupt("not a DriveDex index (bad magic)"))
	}
	hlen := binary.LittleEndian.Uint32(fixed[6:10])
	if hlen > 1<<20 { // headers are tiny; a megabyte means garbage
		return h, fmt.Errorf("%s: %w", path, corrupt("implausible header length %d", hlen))
	}
	hj := make([]byte, hlen)
	if _, err := io.ReadFull(f, hj); err != nil {
		return h, fmt.Errorf("%s: %w", path, corrupt("header cut short"))
	}
	if err := json.Unmarshal(hj, &h); err != nil {
		return h, fmt.Errorf("%s: %w", path, corrupt("unreadable header: %v", err))
	}
	return h, nil
}

var errTruncated = errors.New("file ends in the middle of an entry")

// reader decodes numbers from a byte slice. After the first problem it stays
// in the error state and returns zeros, so the parsing loop can stay simple
// and check r.err once.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		r.err = errTruncated
		return 0
	}
	r.off += n
	return v
}

func (r *reader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b[r.off:])
	if n <= 0 {
		r.err = errTruncated
		return 0
	}
	r.off += n
	return v
}

func (r *reader) byte() byte {
	if r.err != nil {
		return 0
	}
	if r.off >= len(r.b) {
		r.err = errTruncated
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}
