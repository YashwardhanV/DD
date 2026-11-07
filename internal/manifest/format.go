package manifest

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"strings"
)

// manifest.ddm layout:
//
//	"DDM1"
//	uint32 LE  header length
//	JSON header {format_version, written_by, dst_fs, record_count, dir_count}
//	gzip stream of:
//	    uvarint n, n source IDs (strings, interned)
//	    uvarint n, n folder paths (each folder once)
//	    uvarint n, n records (folder index + name, then the fields)
//	uint32 LE  CRC-32C of the decompressed stream
//
// Strings are uvarint length + bytes.

var magic = []byte("DDM1")

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt means the file is not a manifest or was damaged.
var ErrCorrupt = errors.New("the copy manifest is damaged")

// Limits that keep a damaged or hostile file from asking for huge allocations.
const (
	maxHeader = 1 << 20
	maxString = 1 << 16
)

// Encode writes m in the manifest.ddm format.
func Encode(w io.Writer, m *Manifest, writtenBy string) error {
	// Folder table and interned source IDs, in a stable order.
	keys := make([]string, 0, len(m.recs))
	for k := range m.recs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dirIdx, srcIdx := map[string]int{}, map[string]int{}
	var dirs, srcs []string
	for _, k := range keys {
		d, _ := splitKey(k)
		if _, ok := dirIdx[d]; !ok {
			dirIdx[d] = len(dirs)
			dirs = append(dirs, d)
		}
		s := m.recs[k].SrcID
		if _, ok := srcIdx[s]; !ok {
			srcIdx[s] = len(srcs)
			srcs = append(srcs, s)
		}
	}

	var body bytes.Buffer
	e := &enc{w: &body}
	e.uvarint(uint64(len(srcs)))
	for _, s := range srcs {
		e.str(s)
	}
	e.uvarint(uint64(len(dirs)))
	for _, d := range dirs {
		e.str(d)
	}
	e.uvarint(uint64(len(keys)))
	for _, k := range keys {
		r := m.recs[k]
		d, name := splitKey(k)
		e.uvarint(uint64(dirIdx[d]))
		e.str(name)
		e.uvarint(uint64(srcIdx[r.SrcID]))
		e.fields(r)
	}

	h := m.Header
	h.FormatVersion, h.WrittenBy, h.RecordCount, h.DirCount = FormatVersion, writtenBy, len(keys), len(dirs)
	hj, err := json.Marshal(h)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	bw.Write(magic)
	binary.Write(bw, binary.LittleEndian, uint32(len(hj)))
	bw.Write(hj)
	zw := gzip.NewWriter(bw)
	if _, err := zw.Write(body.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	binary.Write(bw, binary.LittleEndian, crc32.Checksum(body.Bytes(), castagnoli))
	return bw.Flush()
}

// Decode reads a manifest.ddm.
func Decode(r io.Reader) (*Manifest, error) {
	br := bufio.NewReader(r)
	var mg [4]byte
	if _, err := io.ReadFull(br, mg[:]); err != nil || !bytes.Equal(mg[:], magic) {
		return nil, fmt.Errorf("%w: not a manifest file", ErrCorrupt)
	}
	var hl uint32
	if err := binary.Read(br, binary.LittleEndian, &hl); err != nil || hl > maxHeader {
		return nil, fmt.Errorf("%w: bad header length", ErrCorrupt)
	}
	hj := make([]byte, hl)
	if _, err := io.ReadFull(br, hj); err != nil {
		return nil, fmt.Errorf("%w: short header", ErrCorrupt)
	}
	m := New()
	if err := json.Unmarshal(hj, &m.Header); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrCorrupt, err)
	}
	if m.Header.FormatVersion < 1 || m.Header.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("manifest format version %d is newer than this DriveDex understands; update DriveDex", m.Header.FormatVersion)
	}
	// The rest is the gzip stream followed by 4 bytes of CRC. gzip stops at
	// the end of its own stream, so the CRC is what is left in br afterwards.
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	zr.Multistream(false)
	body, err := io.ReadAll(io.LimitReader(zr, 1<<34))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	var crc uint32
	if err := binary.Read(br, binary.LittleEndian, &crc); err != nil || crc != crc32.Checksum(body, castagnoli) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}

	d := &dec{b: body}
	srcs := d.strs()
	dirs := d.strs()
	n := d.count()
	for i := 0; i < n && d.err == nil; i++ {
		di := d.uvarint()
		name := d.str()
		si := d.uvarint()
		var rec Record
		d.fields(&rec)
		if d.err != nil {
			break
		}
		if di >= uint64(len(dirs)) || si >= uint64(len(srcs)) || name == "" {
			return nil, fmt.Errorf("%w: bad record %d", ErrCorrupt, i)
		}
		rec.Rel = joinKey(dirs[di], name)
		rec.SrcID = srcs[si]
		m.recs[rec.Rel] = rec
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, d.err)
	}
	if len(d.b) != 0 {
		return nil, fmt.Errorf("%w: trailing data", ErrCorrupt)
	}
	return m, nil
}

func splitKey(k string) (dir, name string) {
	if i := strings.LastIndexByte(k, '\\'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

func joinKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + `\` + name
}

// --- field encoding ---------------------------------------------------------

type enc struct{ w *bytes.Buffer }

func (e *enc) uvarint(v uint64) {
	var b [binary.MaxVarintLen64]byte
	e.w.Write(b[:binary.PutUvarint(b[:], v)])
}

func (e *enc) varint(v int64) {
	var b [binary.MaxVarintLen64]byte
	e.w.Write(b[:binary.PutVarint(b[:], v)])
}

func (e *enc) str(s string) {
	e.uvarint(uint64(len(s)))
	e.w.WriteString(s)
}

// fields writes everything of a record except its path and source ID.
func (e *enc) fields(r Record) {
	e.str(r.SrcRel)
	e.uvarint(r.SrcFileID)
	e.varint(r.SrcSize)
	e.varint(r.SrcMTimeNs)
	e.varint(r.DstSize)
	e.varint(r.DstMTimeNs)
	e.w.WriteByte(r.HashKind)
	if r.HashKind != HashNone {
		e.w.Write(r.Hash[:])
	}
	e.varint(r.SrcUSN)
	e.uvarint(r.JournalID)
	e.varint(r.CopiedAt)
	e.w.WriteByte(r.Flags)
}

type dec struct {
	b   []byte
	err error
}

var errShort = errors.New("truncated")

func (d *dec) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errShort
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = errShort
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) byte1() byte {
	if d.err != nil {
		return 0
	}
	if len(d.b) < 1 {
		d.err = errShort
		return 0
	}
	c := d.b[0]
	d.b = d.b[1:]
	return c
}

func (d *dec) str() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > maxString || n > uint64(len(d.b)) {
		d.err = errShort
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// count reads a list length, refusing one that can't fit in what is left
// (every element takes at least one byte).
func (d *dec) count() int {
	n := d.uvarint()
	if n > uint64(len(d.b)) {
		d.err = errShort
		return 0
	}
	return int(n)
}

func (d *dec) strs() []string {
	n := d.count()
	out := make([]string, 0, n)
	for i := 0; i < n && d.err == nil; i++ {
		out = append(out, d.str())
	}
	return out
}

func (d *dec) fields(r *Record) {
	r.SrcRel = d.str()
	r.SrcFileID = d.uvarint()
	r.SrcSize = d.varint()
	r.SrcMTimeNs = d.varint()
	r.DstSize = d.varint()
	r.DstMTimeNs = d.varint()
	r.HashKind = d.byte1()
	if r.HashKind != HashNone && d.err == nil {
		if len(d.b) < 16 {
			d.err = errShort
			return
		}
		copy(r.Hash[:], d.b[:16])
		d.b = d.b[16:]
	}
	r.SrcUSN = d.varint()
	r.JournalID = d.uvarint()
	r.CopiedAt = d.varint()
	r.Flags = d.byte1()
}
