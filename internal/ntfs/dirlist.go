package ntfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// Why read sizes and dates folder by folder
//
// Reading the MFT gives every name in seconds, but not sizes or dates. Asking
// for them one file at a time (os.Lstat) costs a system call per file: a
// million calls for a million files. Windows can instead list a whole folder
// with one call (GetFileInformationByHandleEx with FileIdBothDirectoryInfo),
// and each record it returns carries the file's reference number - the very
// ID the index uses - plus its size and last-write time. So filling a turbo
// index costs one call per FOLDER, and the result is matched to the index by
// ID, not by name.

// DirRecord is one entry of a folder listing.
type DirRecord struct {
	FileID     uint64 // the NTFS file reference number (= index ID)
	Name       string
	Size       int64 // EndOfFile
	MTimeNs    int64 // LastWriteTime, nanoseconds since 1970 (UTC)
	Attrs      uint32
	ReparseTag uint32 // for reparse points (FILE_ATTRIBUTE_REPARSE_POINT), from EaSize
}

// DirLister lists one folder (an absolute path such as `E:\Photos`).
type DirLister interface {
	ListDir(dir string) ([]DirRecord, error)
}

// FileStater is a DirLister that can also look up one file (the few entries
// a journal refresh touched). Listers without it use os.Lstat.
type FileStater interface {
	StatFile(path string) (DirRecord, error)
}

// statFile looks up one file through the lister if it can, else os.Lstat.
func statFile(lister DirLister, path string) (DirRecord, error) {
	if fs, ok := lister.(FileStater); ok {
		return fs.StatFile(path)
	}
	fi, err := lstat(path)
	if err != nil {
		return DirRecord{}, err
	}
	rec := DirRecord{Name: fi.Name(), Size: fi.Size(), MTimeNs: fi.ModTime().UnixNano()}
	if fi.IsDir() {
		rec.Attrs |= AttrDirectory
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		rec.Attrs |= AttrReparse // a link: Go's view of "not a regular file"
	}
	return rec, nil
}

// Reparse tags that matter for copying.
const (
	// TagDedup: a Data Deduplication file. Windows (and Go) read it as a
	// normal file; it is copied.
	TagDedup uint32 = 0x80000013
)

// isLink reports whether an entry with these attributes and reparse tag is
// a link DriveDex must not copy: any reparse point except a deduplicated
// file. That is exactly what Go's os.Lstat reports as "not a regular file"
// on Windows (symlinks, junctions, cloud placeholders, app links...), so an
// index-planned copy skips the same entries as a fresh listing.
func isLink(attrs, tag uint32) bool {
	return attrs&AttrReparse != 0 && tag != TagDedup
}

// fileTimeToUnixNs converts a FILETIME (100-ns steps since 1601) to Unix ns.
func fileTimeToUnixNs(ft int64) int64 {
	const epochDiff = 116444736000000000 // 1601-01-01 to 1970-01-01 in 100 ns
	return (ft - epochDiff) * 100
}

// FILE_ID_BOTH_DIR_INFO layout (64-bit Windows): offsets of the fields read.
const (
	fidNextEntry  = 0
	fidLastWrite  = 24
	fidEndOfFile  = 40
	fidAttributes = 56
	fidNameLength = 60
	fidEaSize     = 64
	fidFileID     = 96
	fidFileName   = 104
)

// ErrBadDirInfo means a folder listing buffer doesn't parse.
var ErrBadDirInfo = errors.New("malformed folder listing")

// ParseDirInfo decodes a buffer of FILE_ID_BOTH_DIR_INFO records, appending
// them to recs. "." and ".." are skipped. Damaged input gives an error, never
// a panic (it is fuzzed).
func ParseDirInfo(recs []DirRecord, buf []byte) ([]DirRecord, error) {
	le := binary.LittleEndian
	off := 0
	for off < len(buf) {
		if off+fidFileName > len(buf) {
			return recs, fmt.Errorf("%w: record at %d is cut short", ErrBadDirInfo, off)
		}
		r := buf[off:]
		next := int(le.Uint32(r[fidNextEntry:]))
		nameLen := int(le.Uint32(r[fidNameLength:]))
		if nameLen%2 != 0 || fidFileName+nameLen > len(r) || (next != 0 && next < fidFileName+nameLen) {
			return recs, fmt.Errorf("%w: bad lengths at %d", ErrBadDirInfo, off)
		}
		u := make([]uint16, nameLen/2)
		for i := range u {
			u[i] = le.Uint16(r[fidFileName+2*i:])
		}
		name := string(utf16.Decode(u))
		if name != "." && name != ".." {
			rec := DirRecord{
				FileID:  le.Uint64(r[fidFileID:]),
				Name:    name,
				Size:    int64(le.Uint64(r[fidEndOfFile:])),
				MTimeNs: fileTimeToUnixNs(int64(le.Uint64(r[fidLastWrite:]))),
				Attrs:   le.Uint32(r[fidAttributes:]),
			}
			if rec.Attrs&AttrReparse != 0 {
				rec.ReparseTag = le.Uint32(r[fidEaSize:])
			}
			recs = append(recs, rec)
		}
		if next == 0 {
			break
		}
		off += next
	}
	return recs, nil
}

// EncodeDirInfo builds a FILE_ID_BOTH_DIR_INFO buffer (tests and the fake volume).
func EncodeDirInfo(recs []DirRecord) []byte {
	le := binary.LittleEndian
	var out []byte
	for i, rec := range recs {
		name := utf16.Encode([]rune(rec.Name))
		size := fidFileName + 2*len(name)
		size = (size + 7) &^ 7
		r := make([]byte, size)
		if i < len(recs)-1 {
			le.PutUint32(r[fidNextEntry:], uint32(size))
		}
		ft := rec.MTimeNs/100 + 116444736000000000
		le.PutUint64(r[fidLastWrite:], uint64(ft))
		le.PutUint64(r[fidEndOfFile:], uint64(rec.Size))
		le.PutUint32(r[fidAttributes:], rec.Attrs)
		le.PutUint32(r[fidNameLength:], uint32(2*len(name)))
		le.PutUint32(r[fidEaSize:], rec.ReparseTag)
		le.PutUint64(r[fidFileID:], rec.FileID)
		for j, c := range name {
			le.PutUint16(r[fidFileName+2*j:], c)
		}
		out = append(out, r...)
	}
	return out
}

// fill stores one listing record into the index slot it belongs to.
func fill(ix *index.Index, slot int, rec DirRecord) {
	if ix.Flags[slot]&index.FlagDir != 0 {
		ix.Sizes[slot] = 0
	} else {
		ix.Sizes[slot] = rec.Size
	}
	ix.MTimes[slot] = floorDiv(rec.MTimeNs, 1e9)
	ix.Nanos[slot] = uint32(rec.MTimeNs - ix.MTimes[slot]*1e9)
	f := ix.Flags[slot] &^ (index.FlagNoMeta | index.FlagReparse)
	if isLink(rec.Attrs, rec.ReparseTag) {
		f |= index.FlagReparse
	}
	ix.Flags[slot] = f
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// MetaStats is what a metadata fill did.
type MetaStats struct {
	Folders   int64 // folders listed
	Filled    int64 // entries that got their size and time
	Missing   int64 // entries still without (their folder couldn't be read)
	Unreadble int64 // folders that couldn't be opened
}

// FillMetadataByDir gives every entry its size, exact modification time and
// link flag by listing each folder once (see the comment at the top of this
// file). root is the drive root such as `E:\`. Folders that can't be opened
// leave their entries with FlagNoMeta. Afterwards the header says whether the
// index is complete (MetaComplete) and that its times are exact (MTimeUnit).
func FillMetadataByDir(ctx context.Context, ix *index.Index, root string, lister DirLister, workers int, progress func(done, total int64)) (MetaStats, error) {
	// Every folder that holds entries (the root is folder 0).
	hasChildren := map[uint64]bool{}
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Flags[slot]&index.FlagDeleted == 0 {
			hasChildren[ix.Parents[slot]] = true
		}
	}
	folders := make([]uint64, 0, len(hasChildren))
	for id := range hasChildren {
		folders = append(folders, id)
	}
	sort.Slice(folders, func(a, b int) bool { return folders[a] < folders[b] })
	st, err := fillFolders(ctx, ix, root, lister, folders, nil, workers, progress)
	finishMeta(ix)
	return st, err
}

// fillFolders lists the given folders and fills their children. With only
// non-nil, just those IDs are filled (a journal refresh).
func fillFolders(ctx context.Context, ix *index.Index, root string, lister DirLister, folders []uint64, only map[uint64]bool, workers int, progress func(done, total int64)) (MetaStats, error) {
	if workers < 1 {
		workers = 1
	}
	var st MetaStats
	var filled, folderCount, bad atomic.Int64
	jobs := make(chan uint64)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache := map[uint64]string{}
			for id := range jobs {
				if ctx.Err() != nil {
					continue
				}
				dir := root
				if id != 0 {
					slot, ok := ix.Slot(id)
					if !ok {
						continue
					}
					dir = index.JoinRoot(root, ix.RelPathCached(slot, cache))
				}
				recs, err := lister.ListDir(dir)
				if n := folderCount.Add(1); progress != nil && n%256 == 0 {
					progress(n, int64(len(folders)))
				}
				if err != nil {
					bad.Add(1)
					continue
				}
				for _, rec := range recs {
					slot, ok := ix.Slot(rec.FileID)
					// Match by ID AND parent: each slot has one parent, so the
					// workers never write the same slot.
					if !ok || ix.Parents[slot] != id || (only != nil && !only[rec.FileID]) {
						continue
					}
					fill(ix, slot, rec)
					filled.Add(1)
				}
			}
		}()
	}
	for _, id := range folders {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	st.Folders, st.Filled, st.Unreadble = folderCount.Load(), filled.Load(), bad.Load()
	for slot := 0; slot < ix.Len(); slot++ {
		if f := ix.Flags[slot]; f&index.FlagNoMeta != 0 && f&index.FlagDeleted == 0 && (only == nil || only[ix.IDs[slot]]) {
			st.Missing++
		}
	}
	return st, ctx.Err()
}

// finishMeta sets the header fields that describe the metadata.
func finishMeta(ix *index.Index) {
	ix.Header.MetaComplete = MetaComplete(ix)
	ix.Header.MTimeUnit = "ns"
}

// MetaComplete reports whether every live entry has its size and time.
func MetaComplete(ix *index.Index) bool {
	for slot := 0; slot < ix.Len(); slot++ {
		if f := ix.Flags[slot]; f&index.FlagNoMeta != 0 && f&index.FlagDeleted == 0 {
			return false
		}
	}
	return true
}

// touchedThreshold: a folder with this many changed entries is listed in one
// call; fewer are looked up one by one.
const touchedThreshold = 32

// lstat is os.Lstat (a variable so tests count calls).
var lstat = os.Lstat

// RefreshTouched fills the size and time of entries the change journal
// touched (Applier.Touched), so an index stays complete after a refresh
// without re-reading the drive. Changed entries are grouped by folder: a
// folder with 32 or more of them is listed once, the rest are looked up one
// by one. Entries that were deleted simply aren't in the index any more.
func RefreshTouched(ctx context.Context, ix *index.Index, root string, touched []uint64, lister DirLister, workers int) (MetaStats, error) {
	byParent := map[uint64][]int{}
	only := map[uint64]bool{}
	for _, id := range touched {
		slot, ok := ix.Slot(id)
		if !ok {
			continue // deleted
		}
		byParent[ix.Parents[slot]] = append(byParent[ix.Parents[slot]], slot)
		only[id] = true
	}
	var listed []uint64
	var single []int
	for parent, slots := range byParent {
		if len(slots) >= touchedThreshold && lister != nil {
			listed = append(listed, parent)
		} else {
			single = append(single, slots...)
		}
	}
	st, err := fillFolders(ctx, ix, root, lister, listed, only, workers, nil)
	if err != nil {
		return st, err
	}
	cache := map[uint64]string{}
	for _, slot := range single {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		rec, err := statFile(lister, index.JoinRoot(root, ix.RelPathCached(slot, cache)))
		if err != nil {
			continue // gone again since, or unreadable: keeps FlagNoMeta
		}
		rec.FileID = ix.IDs[slot]
		fill(ix, slot, rec)
		st.Filled++
	}
	st.Missing = 0
	for id := range only {
		if slot, ok := ix.Slot(id); ok && ix.Flags[slot]&index.FlagNoMeta != 0 {
			st.Missing++
		}
	}
	finishMeta(ix)
	return st, nil
}

// ---------------------------------------------------------------------------
// Proving an index is fresh
// ---------------------------------------------------------------------------

// FreshEnv is what Fresh needs to know and may do.
type FreshEnv struct {
	// Facts the caller established (condition 1 of the design, and 2).
	Mounted       bool   // the volume is connected now
	SerialMatches bool   // ...and it is the indexed volume
	FileSystem    string // its file system now
	Elevated      bool   // the process can read the change journal
	Root          string // `E:\`

	// Open opens the volume for journal reading (ntfs.Open on Windows).
	Open func(root string) (*Session, error)
	// Lister reads folders (metadata refresh).
	Lister DirLister
	// Save writes the refreshed index (under the store lock, held by the caller).
	Save func(*index.Index) error
	// FolderID is the folder being copied (0 = the whole drive): only entries
	// under it must have complete metadata.
	FolderID uint64
	Workers  int
}

// FreshResult explains the outcome.
type FreshResult struct {
	Fresh   bool
	Why     string // the first condition that failed, in plain English ("" when fresh)
	Changes int    // entries the journal replay changed
	Took    time.Duration
}

// lstatFillMax: missing metadata for fewer entries than this is fetched one
// by one; more means a normal listing is quicker.
const lstatFillMax = 1000

// Fresh proves that ix describes the drive exactly as it is now, so a copy may
// be planned from it instead of listing the source again (INV-5). It returns
// Fresh=true only if every condition holds; otherwise Why names the first one
// that failed. On the way it replays the change journal into ix (and saves it).
//
//  1. It is an NTFS volume that is connected now, the same one (serial).
//  2. This process can read the change journal (Administrator).
//  3. The journal is the one the index was made with and still reaches back
//     to the index's bookmark.
//  4. Replaying the journal and refreshing the changed entries succeeds; the
//     result is saved.
//  5. The index skipped no folders beyond the system ones a copy skips too.
//  6. No entry under the copied folder lacks its size or time (a few are
//     looked up; many mean "list it instead").
func Fresh(ctx context.Context, ix *index.Index, env FreshEnv) (FreshResult, error) {
	start := time.Now()
	h := &ix.Header
	no := func(why string) (FreshResult, error) { return FreshResult{Why: why, Took: time.Since(start)}, nil }

	// 1
	switch {
	case h.SourceKind != index.KindVolume:
		return no("the saved index is not of a whole drive")
	case !strings.EqualFold(h.FileSystem, "NTFS") || !strings.EqualFold(env.FileSystem, "NTFS"):
		return no("the drive is not NTFS, so it has no change journal to check the index against")
	case !env.Mounted || !env.SerialMatches:
		return no("the indexed drive is not the one connected")
	case h.Scanner != "mft":
		return no("the index was not made in NTFS turbo mode (run 'drivedex index " + letterOf(env.Root) + "' as Administrator)")
	}
	// 2
	if !env.Elevated || env.Open == nil {
		return no("not running as Administrator")
	}
	sess, err := env.Open(env.Root)
	if err != nil {
		return no("the change journal can't be read (" + err.Error() + ")")
	}
	defer sess.Close()
	// 3
	jd, err := QueryJournal(sess.Dev)
	if err != nil {
		return no("the drive has no active change journal")
	}
	switch {
	case h.UsnJournalID == 0 || h.UsnJournalID != jd.ID:
		return no("the change journal was reset since the drive was indexed")
	case h.NextUsn < jd.LowestValidUsn:
		return no("the change journal no longer reaches back to when the drive was indexed")
	}
	// 4
	ap := NewApplier(ix, sess.RootFRN, skipNamesOf(h))
	next, err := ReadJournal(ctx, sess.Dev, jd.ID, h.NextUsn, ap.Apply)
	if err != nil {
		if ctx.Err() != nil {
			return FreshResult{}, ctx.Err()
		}
		return no("replaying the change journal failed (" + err.Error() + ")")
	}
	h.NextUsn = next
	if _, err := RefreshTouched(ctx, ix, env.Root, ap.Touched(), env.Lister, max(env.Workers, 4)); err != nil {
		if ctx.Err() != nil {
			return FreshResult{}, ctx.Err()
		}
		return no("refreshing sizes and dates failed (" + err.Error() + ")")
	}
	ix.Compact()
	h.UpdatedAt = time.Now()
	if env.Save != nil {
		if err := env.Save(ix); err != nil {
			return no("the refreshed index couldn't be saved (" + err.Error() + ")")
		}
	}
	// 5
	if len(h.SkipRules) == 0 {
		return no("the index doesn't record which folders it skipped (it was made by an older DriveDex; re-index the drive)")
	}
	allowed := map[string]bool{}
	for _, n := range scan.SkipRules(nil) {
		allowed[n] = true
	}
	for _, n := range h.SkipRules {
		if !allowed[n] {
			return no(fmt.Sprintf("the index leaves out folders named %q (skip_dirs_extra), which a copy must include", n))
		}
	}
	// 6
	var missing []int
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Flags[slot]&index.FlagNoMeta != 0 && ix.Under(slot, env.FolderID) {
			missing = append(missing, slot)
			if len(missing) >= lstatFillMax {
				return no("sizes and dates are missing for many files (re-index the drive with metadata)")
			}
		}
	}
	cache := map[uint64]string{}
	for _, slot := range missing {
		rec, err := statFile(env.Lister, index.JoinRoot(env.Root, ix.RelPathCached(slot, cache)))
		if err != nil {
			return no("a file's size and date couldn't be read (" + ix.RelPathCached(slot, cache) + ")")
		}
		rec.FileID = ix.IDs[slot]
		fill(ix, slot, rec)
	}
	return FreshResult{Fresh: true, Changes: ap.Changes(), Took: time.Since(start)}, nil
}

// skipNamesOf is the skip list an index was built with (nil = the defaults).
func skipNamesOf(h *index.Header) []string {
	if len(h.SkipRules) == 0 {
		return nil
	}
	return h.SkipRules
}

func letterOf(root string) string {
	if len(root) >= 2 && root[1] == ':' {
		return strings.ToUpper(root[:2])
	}
	return root
}

// SetLstatForTest makes RefreshTouched call onCall before each single lookup
// (tests count them). It is undone when the test ends.
func SetLstatForTest(t interface{ Cleanup(func()) }, onCall func()) {
	old := lstat
	lstat = func(name string) (os.FileInfo, error) { onCall(); return old(name) }
	t.Cleanup(func() { lstat = old })
}
