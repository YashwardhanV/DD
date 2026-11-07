package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/manifest"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/preflight"
	"github.com/yashwardhanv/dd/internal/trash"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// destBook is a run's bookkeeping on the destination: the copy manifest (what
// DriveDex placed there, and from where) and the trash run for files it
// replaces or removes.
type destBook struct {
	dst   string
	store *manifest.Store    // nil: no manifest is written (dry run, --no-manifest, can't)
	m     *manifest.Manifest // never nil (empty when there is none)
	trash *trash.Run         // nil: replaced files are not kept
	srcID string
	jv    *copier.JournalView
	now   time.Time
	notes []preflight.Issue // Info issues for the pre-flight report

	kind, srcArg string
	off          bool // no manifest is written this run
}

// openBook reads dst's manifest (without changing anything: a copy that
// pre-flight stops must leave the destination untouched). lock() takes it for
// writing once the run goes ahead.
func openBook(out io.Writer, dst, srcID, kind, srcArg string, dryRun, noManifest bool) (*destBook, error) {
	b := &destBook{dst: dst, m: manifest.New(), srcID: srcID, now: time.Now(), kind: kind, srcArg: srcArg, off: dryRun || noManifest}
	if noManifest {
		b.notes = append(b.notes, preflight.Issue{Code: "no-manifest", Severity: preflight.Info,
			Message: "No copy manifest is kept (--no-manifest): the next run compares by size and time only."})
	}
	if noManifest {
		return b, nil
	}
	m, err := manifest.Read(dst)
	if err != nil {
		b.notes = append(b.notes, preflight.Issue{Code: "no-manifest", Severity: preflight.Info,
			Message: "The copy manifest on this destination can't be read (" + firstLineOf(err.Error()) + "); files are compared by size and time, and it is started again."})
		return b, nil
	}
	b.m = m
	return b, nil
}

// lock takes the manifest for writing (creating <dst>\.drivedex), purges old
// trash runs and starts this run's trash. Another DriveDex writing to the same
// destination is an error; any other problem means "no manifest this time".
func (b *destBook) lock(out io.Writer) error {
	if b.off || b.store != nil {
		return nil
	}
	s, err := manifest.Open(b.dst)
	switch {
	case errors.Is(err, manifest.ErrLocked):
		return &UserError{Msg: "another DriveDex copy or sync is writing to " + b.dst + " right now",
			Hint: "wait for it to finish, then run this again", Err: err, Code: exitBlocked}
	case err != nil:
		fmt.Fprintln(out, ui.Dim("note: no copy manifest can be kept on this destination ("+firstLineOf(err.Error())+"); files are compared by size and time."))
		b.off = true
		return nil
	}
	b.store, b.m = s, s.Manifest()
	if n, torn := s.Recovered(); n > 0 || torn {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Recovered %s copy records from an interrupted run.", ui.Thousands(int64(n)))))
	}
	b.purgeTrash(out)
	if activeConfig.KeepReplaced || b.kind == "sync" { // a sync never deletes: removed files always go to the trash
		b.trash = trash.Begin(b.dst, b.kind, b.srcArg, b.now)
	}
	return nil
}

// keepsTrash reports whether replaced files will go to the trash.
func (b *destBook) keepsTrash() bool {
	return !b.off && (activeConfig.KeepReplaced || b.kind == "sync")
}

// purgeTrash deletes trash runs older than trash_keep (after one line saying so).
func (b *destBook) purgeTrash(out io.Writer) {
	keep, err := activeConfig.TrashKeepDuration()
	if err != nil || keep <= 0 {
		return
	}
	gone, err := trash.Empty(b.dst, keep, b.now)
	if err != nil || len(gone) == 0 {
		return
	}
	var files int
	var bytes int64
	for _, r := range gone {
		files += r.Files()
		bytes += r.Bytes()
	}
	fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Emptied %d trash %s older than %s on this destination (%s files, %s).",
		len(gone), plural(len(gone), "run", "runs"), activeConfig.TrashKeep, ui.Thousands(int64(files)), ui.HumanSize(bytes))))
}

// options adds the manifest and trash hooks to copy options.
func (b *destBook) options(opt copier.Options) copier.Options {
	if b.store != nil {
		opt.Hash = true // the record keeps the content hash; XXH3 costs next to nothing
		opt.Record = b.record
	}
	if b.trash != nil {
		opt.Trash = b.trash
	}
	return opt
}

// record writes the manifest record of a file now in place.
func (b *destBook) record(it *copier.Item) {
	r := manifest.Record{
		Rel: it.Rel, SrcID: b.srcID, SrcFileID: it.SrcFileID,
		SrcSize: it.SrcSize, SrcMTimeNs: it.SrcMTimeNs, DstSize: it.Dst.Size, DstMTimeNs: it.Dst.MTimeNs,
		CopiedAt: b.now.Unix(),
	}
	if r.SrcSize == 0 && r.SrcMTimeNs == 0 {
		r.SrcSize, r.SrcMTimeNs = it.Size, it.MTimeNs()
	}
	if it.Renamed {
		r.SrcRel, r.Flags = it.SrcRel, r.Flags|manifest.FlagSanitised
	}
	if it.Hashed {
		r.Hash, r.HashKind = it.Hash, manifest.HashXXH3
	}
	if it.MediaChecked {
		r.Flags |= manifest.FlagVerifiedMedia
	}
	if b.jv != nil {
		r.SrcUSN, r.JournalID = b.jv.Next, b.jv.ID
	}
	b.store.Put(r)
}

// afterRun records files found already in place (adopted: same size and
// time, not copied) and moves the journal bookmark of files the journal
// proved unchanged, so journal comparison keeps working next time.
func (b *destBook) afterRun(plan *copier.Plan) {
	if b.store == nil {
		return
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Decision != copier.SkipSame || !it.Dst.Exists {
			continue
		}
		rec, ok := b.m.Get(it.Rel)
		switch {
		case ok && rec.SrcID == b.srcID && it.Why == copier.WhyJournal && b.jv != nil:
			rec.SrcUSN = b.jv.Next
			b.store.Refresh(rec)
		case !ok || rec.SrcID != b.srcID:
			r := manifest.Record{
				Rel: it.Rel, SrcID: b.srcID, SrcFileID: it.SrcFileID, SrcSize: it.Size, SrcMTimeNs: it.MTimeNs(),
				DstSize: it.Dst.Size, DstMTimeNs: it.Dst.MTimeNs, CopiedAt: b.now.Unix(), Flags: manifest.FlagAdopted,
			}
			if b.jv != nil && it.SrcFileID != 0 {
				r.SrcUSN, r.JournalID = b.jv.Next, b.jv.ID
			}
			b.store.Put(r)
		}
	}
}

// close writes the trash run's description and compacts the manifest.
func (b *destBook) close() error {
	var err error
	if b.trash != nil {
		err = b.trash.Close()
	}
	if b.store != nil {
		if cerr := b.store.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// trashed is how many files this run put in the trash.
func (b *destBook) trashed() (int, int64) { return b.trash.Moved() }

// srcIDFor is the manifest source ID of a local path: its drive's volume ID,
// or a folder ID when the drive can't be identified.
func srcIDFor(path string) string {
	if v, err := volumeForPath(path); err == nil && v.Serial != 0 {
		return volume.SourceID(v)
	}
	return index.FolderSourceID(path)
}

// journalStart is the source drive's change journal position taken BEFORE
// the source is listed: every change after it shows in the journal, so it is
// a safe bookmark for the files this run copies or confirms.
type journalStart struct {
	root string
	id   uint64
	next int64
}

// startJournal reads the journal position of the NTFS drive holding src
// (Administrator needed); nil when there is none to read.
func startJournal(src string) *journalStart {
	if driveOf(src) == "" || !isElevated() {
		return nil
	}
	vol, err := volumeForPath(src)
	if err != nil || !vol.Ready || !strings.EqualFold(vol.FileSystem, "NTFS") {
		return nil
	}
	sess, err := openNTFS(vol.Root)
	if err != nil {
		return nil
	}
	defer sess.Close()
	jd, err := ntfs.QueryJournal(sess.Dev)
	if err != nil {
		return nil
	}
	return &journalStart{root: vol.Root, id: jd.ID, next: jd.NextUsn}
}

// journalView reads which files changed since the oldest bookmark among this
// source's records.
func (b *destBook) journalView(ctx context.Context, js *journalStart) {
	if js == nil {
		return
	}
	since := js.next
	b.m.Range(func(r manifest.Record) bool {
		if r.SrcID == b.srcID && r.JournalID == js.id && r.SrcFileID != 0 && r.SrcUSN < since {
			since = r.SrcUSN
		}
		return true
	})
	jv := &copier.JournalView{ID: js.id, Next: js.next, Since: since, Touched: map[uint64]bool{}}
	if since < js.next {
		sess, err := openNTFS(js.root)
		if err != nil {
			return
		}
		defer sess.Close()
		_, touched, err := ntfs.ChangesSince(ctx, sess, js.id, since)
		jv.Touched = touched // nil when the journal doesn't reach back that far
		if err != nil && !errors.Is(err, ntfs.ErrJournalEntryDeleted) {
			return
		}
	}
	b.jv = jv
}

func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// compareLine is the report's "why skipped" line, or "" when everything was
// compared the ordinary way.
func compareLine(c copier.CompareReport, dst string) string {
	if c.ByJournal == 0 && c.BySum == 0 && c.Changed == 0 && c.DstModified == 0 {
		return ""
	}
	var parts []string
	if c.ByJournal > 0 {
		parts = append(parts, ui.Thousands(c.ByJournal)+" unchanged (journal)")
	}
	if c.BySum > 0 {
		parts = append(parts, ui.Thousands(c.BySum)+" unchanged (checksum)")
	}
	if c.BySizeTime > 0 {
		parts = append(parts, ui.Thousands(c.BySizeTime)+" unchanged (size and time)")
	}
	if c.Changed > 0 {
		parts = append(parts, ui.Thousands(c.Changed)+" changed")
	}
	if c.DstModified > 0 {
		parts = append(parts, fmt.Sprintf("%s replaced because the copy on %s had been modified", ui.Thousands(c.DstModified), letterOrRoot(dst)))
	}
	return strings.Join(parts, ", ")
}
