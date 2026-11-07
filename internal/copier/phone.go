package copier

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/scan"
)

// PhoneSource marks a plan whose files are on an Android phone.
type PhoneSource struct {
	ADB    *phone.ADB
	Serial string
	Remote string // the folder or file being copied, e.g. "/sdcard/DCIM/Camera"
	// HashTool is the checksum program found on the phone ("sha256sum",
	// "sha1sum", "md5sum"; see phone.ADB.HashTool). With --verify, each
	// pulled batch is hashed ON THE PHONE and compared with the same
	// algorithm computed on the PC copy. "" = phone copies can't be verified.
	HashTool string
}

const (
	// maxPullFiles and maxPullChars keep one `adb pull` command line well under
	// the Windows limit (about 32,000 characters).
	maxPullFiles = 100
	maxPullChars = 24000
	// pullWorkers: ADB itself is the bottleneck; more parallel pulls don't help.
	pullWorkers = 2

	stagingPrefix = ".ddxpull-"
)

// ErrRemoteNotFound means the requested path does not exist on the phone.
var ErrRemoteNotFound = errors.New("that path doesn't exist on the phone")

// PhonePath converts what the user typed after "phone:" into an absolute path
// on the phone. Paths are relative to the phone's shared storage, the way
// DriveDex shows them ("/DCIM/Camera" means "/sdcard/DCIM/Camera"); paths that
// already start with /sdcard/ or /storage/ are used as they are.
func PhonePath(p string) string { return PhonePathRoot(p, "/sdcard") }

// PlanPhone plans copying a file or folder from the phone into dstRoot.
// remote must be an absolute path on the phone (see PhonePath).
func PlanPhone(ctx context.Context, adb *phone.ADB, serial, remote, dstRoot, policy string, workers int) (*Plan, error) {
	info, err := adb.StatPath(ctx, serial, remote)
	if err != nil {
		return nil, fmt.Errorf("asking the phone about %s: %w", remote, err)
	}
	if !info.Exists {
		return nil, fmt.Errorf("%w: %s", ErrRemoteNotFound, remote)
	}
	src := &PhoneSource{ADB: adb, Serial: serial, Remote: remote}

	name := path.Base(strings.TrimRight(remote, "/"))
	if name == "." || name == "/" || name == "sdcard" {
		name = "Phone"
	}

	if !info.IsDir {
		job := Job{Src: remote, Rel: SafeName(path.Base(remote)), Size: info.Size, ModTime: info.ModTime, TimeCoarse: true}
		plan := finalize(dstRoot, nil, []Job{job}, policy, 1, fsinfo.ForPhone())
		plan.Phone = src
		return plan, nil
	}

	// One remote command lists every file with its size and time (stage 5).
	b := index.NewBuilder(index.Header{})
	sc := phone.NewScanner(adb, serial, remote)
	if err := sc.Scan(ctx, b, func(scan.Progress) {}); err != nil {
		return nil, fmt.Errorf("listing %s on the phone: %w", remote, err)
	}
	ix := b.Index()

	name = SafeName(name)
	dirs := []string{name}
	var jobs []Job
	cache := map[uint64]string{}
	for slot := 0; slot < ix.Len(); slot++ {
		relBack := ix.RelPathCached(slot, cache)
		rel := filepath.Join(name, SafeRel(relBack))
		if ix.Flags[slot]&index.FlagDir != 0 {
			dirs = append(dirs, rel)
			continue
		}
		jobs = append(jobs, Job{
			Src:        path.Join(remote, strings.ReplaceAll(relBack, `\`, "/")),
			Rel:        rel,
			Size:       max(ix.Sizes[slot], 0),
			ModTime:    ix.MTimes[slot],
			TimeCoarse: true, // adb's stat gives whole seconds
		})
	}
	plan := finalize(dstRoot, dirs, jobs, policy, workers, fsinfo.ForPhone())
	plan.Phone = src
	return plan, nil
}

// pullBatch is up to 100 files from ONE phone folder, pulled with one adb call.
type pullBatch struct{ items []*Item }

// makeBatches groups the files to write by their folder on the phone and cuts
// each group into batches that respect the command-line limits.
func makeBatches(p *Plan) []pullBatch {
	groups := map[string][]*Item{}
	var batches []pullBatch
	for i := range p.Items {
		it := &p.Items[i]
		if it.Decision.Skipped() || it.Blocked != "" {
			continue
		}
		if base := path.Base(it.Src); SafeName(base) != base {
			batches = append(batches, pullBatch{items: []*Item{it}}) // pulled alone, to a safe name
			continue
		}
		dir := path.Dir(it.Src)
		groups[dir] = append(groups[dir], it)
	}
	dirs := make([]string, 0, len(groups))
	for d := range groups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, d := range dirs {
		var cur pullBatch
		chars := 0
		for _, it := range groups[d] {
			if len(cur.items) >= maxPullFiles || (chars+len(it.Src) > maxPullChars && len(cur.items) > 0) {
				batches = append(batches, cur)
				cur, chars = pullBatch{}, 0
			}
			cur.items = append(cur.items, it)
			chars += len(it.Src) + 3
		}
		if len(cur.items) > 0 {
			batches = append(batches, cur)
		}
	}
	return batches
}

// runPhone pulls everything the plan needs from the phone.
//
// Safety: `adb pull` writes straight to the name it is given, so an interrupted
// pull would leave a half-written file under its REAL name - and a later run
// might wrongly treat it as already copied. To keep the same guarantee as local
// copies, each batch is pulled into a hidden staging folder and its files are
// renamed into place only when they are complete.
func runPhone(ctx context.Context, p *Plan, opt Options, st *state) error {
	removeStaleStaging(p.DstRoot)
	for i := range p.Items {
		if it := &p.Items[i]; it.Blocked != "" && !it.Decision.Skipped() {
			st.fail(it.Rel, errors.New(it.Blocked))
		}
	}

	queue := make(chan pullBatch)
	var wg sync.WaitGroup
	for w := 0; w < pullWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range queue {
				pullBatchInto(ctx, p, b, opt, st)
			}
		}()
	}
dispatch:
	for _, b := range makeBatches(p) {
		select {
		case queue <- b:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(queue)
	wg.Wait()
	return nil
}

// removeStaleStaging deletes staging folders left by an interrupted run.
func removeStaleStaging(dstRoot string) {
	entries, err := os.ReadDir(dstRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			os.RemoveAll(filepath.Join(dstRoot, e.Name()))
		}
	}
}

// pullBatchInto pulls one batch. Files the batch call didn't deliver are
// retried one at a time (up to 2 more attempts), so one bad file can't sink
// the 99 others.
func pullBatchInto(ctx context.Context, p *Plan, b pullBatch, opt Options, st *state) {
	pending := b.items
	var lastErr error
	for attempt := 0; attempt <= opt.retries() && len(pending) > 0; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			select {
			case <-time.After(opt.retryDelay()):
			case <-ctx.Done():
				return
			}
		}
		// First attempt: the whole batch. Retries: one file per call.
		group := [][]*Item{pending}
		if attempt > 0 {
			group = group[:0]
			for _, it := range pending {
				group = append(group, []*Item{it})
			}
		}
		var stillPending []*Item
		for _, files := range group {
			failed, err := pullAndPlace(ctx, p, files, opt, st)
			if err != nil {
				lastErr = err
			}
			stillPending = append(stillPending, failed...)
		}
		pending = stillPending
	}
	if ctx.Err() != nil {
		return
	}
	for _, it := range pending {
		err := lastErr
		if err == nil {
			err = errors.New("the phone didn't deliver the file")
		}
		st.fail(it.Rel, err)
	}
}

// pullAndPlace pulls files into a fresh staging folder and moves each
// delivered file to its final place. It returns the files that did not arrive.
func pullAndPlace(ctx context.Context, p *Plan, files []*Item, opt Options, st *state) (failed []*Item, err error) {
	staging, mkErr := os.MkdirTemp(p.DstRoot, stagingPrefix)
	if mkErr != nil {
		return files, fmt.Errorf("creating a staging folder in %s: %w", p.DstRoot, mkErr)
	}
	defer os.RemoveAll(staging)

	srcs := make([]string, len(files))
	for i, it := range files {
		srcs[i] = it.Src
	}
	// A name Windows can't store (such as "a:b.jpg") is pulled on its own, to
	// an explicit safe local name; makeBatches gives such files a batch each.
	unsafe := len(files) == 1 && SafeName(path.Base(files[0].Src)) != path.Base(files[0].Src)
	if unsafe {
		err = p.Phone.ADB.PullFile(ctx, p.Phone.Serial, files[0].Src, filepath.Join(staging, SafeName(path.Base(files[0].Src))))
	} else {
		err = p.Phone.ADB.Pull(ctx, p.Phone.Serial, srcs, staging)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var placed []placedFile
	for _, it := range files {
		staged := filepath.Join(staging, path.Base(it.Src))
		if unsafe {
			staged = filepath.Join(staging, SafeName(path.Base(it.Src)))
		}
		info, statErr := os.Stat(staged)
		if statErr != nil || info.IsDir() {
			failed = append(failed, it)
			continue
		}
		t := time.Unix(it.ModTime, 0)
		os.Chtimes(staged, t, t) // keep the phone's modification time (so a re-run sees "same")
		final := filepath.Join(p.DstRoot, it.Rel)
		os.MkdirAll(filepath.Dir(final), 0o755)
		if rerr := os.Rename(staged, final); rerr != nil {
			failed = append(failed, it)
			err = fmt.Errorf("moving %s into place: %w", it.Rel, rerr)
			continue
		}
		it.SrcSize, it.SrcMTimeNs, it.DstMTimeNs = it.Size, it.MTimeNs(), it.MTimeNs()
		placed = append(placed, placedFile{it: it, path: final, size: info.Size()})
	}

	// Verify the batch: one command hashes every file on the phone, the PC
	// hashes its copies (read back from the drive) with the same algorithm.
	bad := map[*Item]bool{}
	if opt.Verify != VerifyOff && p.Phone.HashTool != "" && len(placed) > 0 {
		bad, err = verifyPhoneBatch(ctx, p, placed, opt, st, err)
	}
	for _, pf := range placed {
		if bad[pf.it] {
			failed = append(failed, pf.it)
			continue
		}
		st.written.Add(1)
		st.bytesDone.Add(pf.size)
		st.filesDone.Add(1)
	}
	return failed, err
}

type placedFile struct {
	it   *Item
	path string
	size int64
}

// verifyPhoneBatch compares the phone's checksums with the copies'. A copy
// that differs (or can't be checked) is deleted and returned as bad, so it is
// pulled again.
func verifyPhoneBatch(ctx context.Context, p *Plan, placed []placedFile, opt Options, st *state, prevErr error) (map[*Item]bool, error) {
	bad := map[*Item]bool{}
	remotes := make([]string, len(placed))
	for i, pf := range placed {
		remotes[i] = pf.it.Src
	}
	sums, herr := p.Phone.ADB.HashFiles(ctx, p.Phone.Serial, p.Phone.HashTool, remotes)
	if herr != nil {
		for _, pf := range placed {
			os.Remove(pf.path)
			bad[pf.it] = true
		}
		return bad, fmt.Errorf("%w: the phone couldn't compute checksums (%v)", errVerify, herr)
	}
	var abuf []byte
	if opt.Verify == VerifyMedia {
		abufp := alignedPool.Get().(*[]byte)
		defer alignedPool.Put(abufp)
		abuf = *abufp
	}
	for _, pf := range placed {
		want, ok := sums[pf.it.Src]
		if !ok {
			continue // the tool couldn't name it unambiguously: not verified, not failed
		}
		got, err := hashFileWith(ctx, pf.path, p.Phone.HashTool, opt.Verify == VerifyMedia, abuf)
		if err != nil || got != want {
			os.Remove(pf.path)
			bad[pf.it] = true
			st.mismatches.Add(1)
			prevErr = fmt.Errorf("%w: the copy of %s differs from the file on the phone", errVerify, pf.it.Src)
			continue
		}
		pf.it.MediaChecked = opt.Verify == VerifyMedia
	}
	return bad, prevErr
}

// hashFileWith hashes a local file with the phone tool's algorithm, read back
// without the cache when unbuffered is set.
func hashFileWith(ctx context.Context, path, tool string, unbuffered bool, abuf []byte) (string, error) {
	h, ok := phone.NewHash(tool)
	if !ok {
		return "", fmt.Errorf("unknown hash tool %s", tool)
	}
	var f *os.File
	var err error
	if unbuffered {
		f, err = openUnbuffered(path)
	} else {
		f, err = os.Open(path)
		abuf = make([]byte, 1<<20)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(abuf)
		h.Write(abuf[:n])
		if rerr == io.EOF || (rerr == nil && n == 0) {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PlanPhoneJobs plans pulling an explicit list of files (for example every
// search match on one phone) into dstRoot. Each Job.Src must be an absolute
// path on the phone.
//
// Each Job.Rel is made safe for Windows (see SafeRel): phone file names may
// contain characters such as ':' that Windows doesn't allow.
func PlanPhoneJobs(adb *phone.ADB, serial string, jobs []Job, dstRoot, policy string, workers int) *Plan {
	jobs = append([]Job(nil), jobs...)
	for i := range jobs {
		jobs[i].Rel = SafeRel(jobs[i].Rel)
		jobs[i].TimeCoarse = true
	}
	plan := finalize(dstRoot, nil, jobs, policy, workers, fsinfo.ForPhone())
	plan.Phone = &PhoneSource{ADB: adb, Serial: serial}
	return plan
}

// PhonePathRoot is PhonePath for a phone whose indexed folder is root (the
// phone_root setting): a path such as "/DCIM/Camera" means root + "/DCIM/Camera".
// Paths that already start with /sdcard/ or /storage/ are used as they are.
func PhonePathRoot(p, root string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p == "/sdcard" || strings.HasPrefix(p, "/sdcard/") || p == "/storage" || strings.HasPrefix(p, "/storage/") {
		return path.Clean(p)
	}
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	if root == "" {
		root = "/sdcard"
	}
	return path.Clean(root + p)
}

// SafeName makes one phone file or folder name storable on Windows. Android
// allows names Windows doesn't: a ':' would even silently write an NTFS
// "alternate data stream" instead of a file. So:
//
//   - < > : " | ? * \ and control characters become '_'
//   - trailing dots and spaces (which Windows strips) become '_'
//   - reserved device names (CON, NUL, COM1, LPT1, ... even with an
//     extension, like "aux.txt") get a '_' in front
//
// A name that is already fine is returned unchanged.
func SafeName(name string) string {
	var b strings.Builder
	changed := false
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*\`, r) {
			b.WriteByte('_')
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	s := name
	if changed {
		s = b.String()
	}
	if t := strings.TrimRight(s, ". "); t != s {
		s = t + strings.Repeat("_", len(s)-len(t))
	}
	if s == "" {
		return "_"
	}
	stem := s
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	switch strings.ToUpper(strings.TrimRight(stem, " ")) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return s
}

// SafeRel applies SafeName to every part of a relative path (separated by
// this OS's separator or by the index's `\`) and joins them with this OS's
// separator.
func SafeRel(rel string) string {
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '\\' || r == filepath.Separator })
	for i, p := range parts {
		parts[i] = SafeName(p)
	}
	return filepath.Join(parts...)
}
