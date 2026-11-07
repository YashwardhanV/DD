package copier

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashwardhanv/dd/internal/manifest"
)

// Compare modes (--compare): when is a destination file up to date?
const (
	// CompareAuto: journal for each file whose preconditions hold, fast otherwise.
	CompareAuto = "auto"
	// CompareFast: same size and SameTime, a daylight-saving hour tolerated.
	CompareFast = "fast"
	// CompareStrict: same size and times equal within the coarser resolution;
	// no daylight-saving tolerance.
	CompareStrict = "strict"
	// CompareJournal: the manifest record and the NTFS change journal prove
	// the source file unchanged since it was copied, and the destination is
	// still what was written. Catches edits that keep the old time.
	CompareJournal = "journal"
	// CompareChecksum: the source's content hash (always computed fresh)
	// equals the destination's (from the manifest when the destination is
	// unchanged since its record, otherwise computed).
	CompareChecksum = "checksum"
)

// ValidCompare reports whether s is a known --compare value.
func ValidCompare(s string) bool {
	switch s {
	case "", CompareAuto, CompareFast, CompareStrict, CompareJournal, CompareChecksum:
		return true
	}
	return false
}

// JournalView is what the source volume's NTFS change journal says, read
// once per run.
type JournalView struct {
	ID   uint64 // the journal's ID now
	Next int64  // the journal's next USN when it was read: new records get it as SrcUSN
	// Since is the oldest USN Touched covers; Touched holds every file
	// reference number with a journal entry after Since. Touched == nil means
	// the journal could not be read back that far (wrapped or recreated).
	Since   int64
	Touched map[uint64]bool
}

// CompareOptions are the inputs of Plan.Compare.
type CompareOptions struct {
	Mode     string
	Manifest *manifest.Manifest // nil = no manifest
	SrcID    string             // the source's index ID (records of other sources are ignored)
	Journal  *JournalView       // nil = not an NTFS source, or not readable (not Administrator)
	Workers  int
}

// CompareReport counts what the comparison found, for the final report.
type CompareReport struct {
	Mode                         string
	ByJournal, BySizeTime, BySum int64 // unchanged, per reason
	Changed                      int64 // looked the same, but the content or journal says otherwise
	DstModified                  int64 // destination copies something else changed since DriveDex wrote them
	Hashed                       int64 // files read to compare checksums (source and destination)
	HashedBytes                  int64
	// JournalNote, when set, says why journal comparison wasn't used.
	JournalNote string
}

// errNoChecksumForPhones: checksum comparison needs both files on this PC.
var errNoChecksumForPhones = errors.New("--compare checksum isn't available for phone sources (it would mean copying every file to hash it); use fast or strict")

// Compare applies the comparison mode to the plan's decisions. It can only
// make the plan copy MORE than the size-and-time rule did, never less, except
// in checksum mode, where identical content is up to date whatever the times
// say (INV-1: when in doubt, copy).
func (p *Plan) Compare(ctx context.Context, c CompareOptions) (CompareReport, error) {
	mode := c.Mode
	if mode == "" {
		mode = CompareAuto
	}
	rep := CompareReport{Mode: mode}
	if mode == CompareChecksum && p.Phone != nil {
		return rep, errNoChecksumForPhones
	}
	useJournal := mode == CompareJournal || mode == CompareAuto
	if useJournal && p.Phone == nil {
		switch {
		case c.Journal == nil:
			if mode == CompareJournal {
				rep.JournalNote = "the source has no readable NTFS change journal (an NTFS drive, run as Administrator); compared by size and time instead"
			}
			useJournal = false
		case c.Journal.Touched == nil:
			rep.JournalNote = "the change journal no longer reaches back to the last copy; compared by size and time instead"
			useJournal = false
		}
	} else {
		useJournal = false
	}
	if c.Manifest == nil {
		useJournal = false
	}

	var hashJobs []int
	for i := range p.Items {
		it := &p.Items[i]
		if it.Blocked != "" || !it.Dst.Exists || it.Dst.IsDir {
			continue
		}
		var rec manifest.Record
		var hasRec bool
		if c.Manifest != nil {
			rec, hasRec = c.Manifest.Get(it.Rel)
			hasRec = hasRec && rec.SrcID == c.SrcID
		}
		dstAsRecorded := hasRec && rec.DstSize == it.Dst.Size && rec.DstMTimeNs == it.Dst.MTimeNs
		if hasRec && !dstAsRecorded {
			it.DstModified = true
		}

		switch mode {
		case CompareStrict:
			if it.DSTShift {
				dec := decideWith(p.OnConflict, it.Job, fakeStat(it.Dst), p.SrcFS, p.DstFS, false)
				it.Decision, it.DSTShift, it.Why = dec.d, false, WhyNone
			}
		case CompareChecksum:
			if it.Dst.Size == it.Size {
				hashJobs = append(hashJobs, i)
			}
			continue
		}

		if useJournal && dstAsRecorded && rec.JournalID == c.Journal.ID && rec.SrcFileID != 0 &&
			rec.SrcUSN >= c.Journal.Since && (it.SrcFileID == 0 || it.SrcFileID == rec.SrcFileID) {
			changed := c.Journal.Touched[rec.SrcFileID] || rec.SrcSize != it.Size
			switch {
			case changed && it.Decision.Skipped():
				// Same size and time, but the journal saw a write: the classic
				// "edited, then the old time put back" (INV-1).
				it.Decision, it.Why, it.Changed, it.DSTShift = Overwrite, WhyNone, true, false
			case !changed && it.Decision == SkipSame:
				it.Why = WhyJournal
			}
		}
	}

	if len(hashJobs) > 0 {
		if err := p.compareChecksums(ctx, hashJobs, c, &rep); err != nil {
			return rep, err
		}
	}
	for i := range p.Items {
		it := &p.Items[i]
		if it.Changed {
			rep.Changed++
		}
		if it.DstModified && !it.Decision.Skipped() {
			rep.DstModified++
		}
		if it.Decision != SkipSame {
			continue
		}
		switch it.Why {
		case WhyJournal:
			rep.ByJournal++
		case WhyChecksum:
			rep.BySum++
		default:
			rep.BySizeTime++
		}
	}
	return rep, nil
}

// compareChecksums hashes the source (always) and the destination (unless
// the manifest has its hash and it is unchanged since) of each listed item.
func (p *Plan) compareChecksums(ctx context.Context, idx []int, c CompareOptions, rep *CompareReport) error {
	workers := c.Workers
	if workers <= 0 {
		workers = 4
	}
	var hashed, bytes atomic.Int64
	next := atomic.Int64{}
	var wg sync.WaitGroup
	for w := 0; w < min(workers, len(idx)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bufp := bufPool.Get().(*[]byte)
			defer bufPool.Put(bufp)
			for {
				k := int(next.Add(1) - 1)
				if k >= len(idx) || ctx.Err() != nil {
					return
				}
				it := &p.Items[idx[k]]
				src, err := hashCached(ctx, it.Src, *bufp)
				if err != nil {
					// Can't read the source now: let the copy try (and report it).
					it.Decision, it.Why = Overwrite, WhyNone
					continue
				}
				hashed.Add(1)
				bytes.Add(it.Size)
				var dst [16]byte
				rec, ok := manifest.Record{}, false
				if c.Manifest != nil {
					rec, ok = c.Manifest.Get(it.Rel)
				}
				if ok && rec.HashKind == manifest.HashXXH3 && rec.DstSize == it.Dst.Size && rec.DstMTimeNs == it.Dst.MTimeNs {
					dst = rec.Hash
				} else {
					dst, err = hashCached(ctx, filepath.Join(p.DstRoot, it.Rel), *bufp)
					if err != nil {
						it.Decision, it.Why = Overwrite, WhyNone
						continue
					}
					hashed.Add(1)
					bytes.Add(it.Dst.Size)
				}
				if src == dst {
					it.Decision, it.Why, it.DSTShift = SkipSame, WhyChecksum, false
				} else {
					it.Changed = it.Decision.Skipped()
					it.Decision, it.Why = Overwrite, WhyNone
				}
			}
		}()
	}
	wg.Wait()
	rep.Hashed, rep.HashedBytes = hashed.Load(), bytes.Load()
	return ctx.Err()
}

// fakeStat turns a DstState back into an os.FileInfo for decide.
func fakeStat(d DstState) os.FileInfo {
	if !d.Exists {
		return nil
	}
	return dstInfo{d}
}

// dstInfo is a DstState seen as an os.FileInfo.
type dstInfo struct{ d DstState }

func (f dstInfo) Name() string { return "" }
func (f dstInfo) Size() int64  { return f.d.Size }
func (f dstInfo) Mode() os.FileMode {
	if f.d.IsDir {
		return os.ModeDir
	}
	return 0
}
func (f dstInfo) ModTime() time.Time { return time.Unix(0, f.d.MTimeNs) }
func (f dstInfo) IsDir() bool        { return f.d.IsDir }
func (f dstInfo) Sys() any           { return nil }
