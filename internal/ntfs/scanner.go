package ntfs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// MFTScanner indexes a whole NTFS drive by reading its master file table. It
// implements scan.Scanner (Name() == "mft"). Folder-by-folder walking is not
// involved at all, which is why it is so much faster.
type MFTScanner struct {
	Session *Session
	// Root is the drive root such as `E:\` (needed only for WithMetadata).
	Root string
	// WithMetadata also stats every entry afterwards (slower, but sizes and
	// dates are then complete; needed for size:/modified: filters).
	WithMetadata bool
	// Workers is the number of goroutines used for WithMetadata (<= 0: 8).
	Workers int
	// SkipDirs overrides scan.DefaultSkipDirs when non-nil.
	SkipDirs []string

	stats       scan.Stats
	build       BuildStats
	journal     bool
	metaFilled  int64
	metaMissing int64
}

// Name implements scan.Scanner.
func (s *MFTScanner) Name() string { return "mft" }

// Stats returns the tally of the last Scan.
func (s *MFTScanner) Stats() scan.Stats { return s.stats }

// Build returns what the last scan kept and dropped.
func (s *MFTScanner) Build() BuildStats { return s.build }

// JournalActive reports whether the drive had a change journal (so the index
// carries a bookmark and can be refreshed incrementally next time).
func (s *MFTScanner) JournalActive() bool { return s.journal }

// MetadataFilled returns how many entries got a real size/date from WithMetadata,
// and how many vanished or couldn't be read.
func (s *MFTScanner) MetadataFilled() (filled, missing int64) { return s.metaFilled, s.metaMissing }

// Scan implements scan.Scanner.
func (s *MFTScanner) Scan(ctx context.Context, b *index.Builder, report func(scan.Progress)) error {
	s.stats, s.build, s.journal, s.metaFilled, s.metaMissing = scan.Stats{}, BuildStats{}, false, 0, 0

	// Remember the journal position BEFORE reading the MFT. Anything that
	// changes while we read is then replayed on the next refresh. Replaying is
	// harmless (it is idempotent), skipping would lose changes.
	high := int64(math.MaxInt64)
	var jd JournalData
	switch jdata, err := QueryJournal(s.Session.Dev); {
	case err == nil:
		jd, high, s.journal = jdata, jdata.NextUsn, true
	case errors.Is(err, ErrJournalNotActive), errors.Is(err, ErrJournalDeleteInProgress):
		// carry on without a journal; EnumerateMFT may still report ErrNoJournal
	default:
		return err
	}

	recs, err := EnumerateMFT(ctx, s.Session.Dev, high, func(n int64) {
		if report != nil {
			report(scan.Progress{Entries: n})
		}
	})
	if err != nil {
		return err
	}

	s.build = AddRecords(b, recs, s.Session.RootFRN, s.SkipDirs)
	s.stats = scan.Stats{Files: s.build.Files, Dirs: s.build.Dirs}
	if report != nil {
		report(scan.Progress{Entries: s.stats.Entries(), Dirs: s.stats.Dirs})
	}

	ix := b.Index()
	if s.journal {
		ix.Header.UsnJournalID, ix.Header.NextUsn = jd.ID, jd.NextUsn
	}

	if s.WithMetadata && ix.Len() > 0 {
		workers := s.Workers
		if workers <= 0 {
			workers = min(16, 2*runtime.NumCPU())
		}
		filled, missing, err := FillMetadata(ctx, ix, s.Root, workers, func(done, total int64) {
			if report != nil {
				report(scan.Progress{Entries: done, Current: fmt.Sprintf("reading sizes and dates (%d of %d)", done, total)})
			}
		})
		if err != nil {
			return err
		}
		s.metaFilled, s.metaMissing = filled, missing
	}
	return nil
}
