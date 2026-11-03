package copier

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
	"github.com/yashwardhanv/dd/internal/phone"
)

// The phone tar lane (BUILD-V2 stage 16)
//
//	phone (read-only)          USB / adb             PC (this file)               writeAtomic
//	cd /sdcard/DCIM &&    -->  exec-out: binary  --> archive/tar: accept only --> x.ddxpart -> mtime
//	tar -cf - -- a b ...       safe, no CR/LF        requested names; refuse      -> rename to x.jpg
//	(one process, one stream)  translation           .. / absolute / links
//
// Up to 2,000 small files travel in one tar stream, so the phone never waits
// for the PC between files. Big files (32 MB and up) don't gain from that and
// keep the v1 pull lane, which can resume per file; so do names Windows can't
// store (the pull lane saves them under a safe name).
//
// Every file still goes through writeAtomic: a file's real name appears only
// when it is complete, exactly like every other DriveDex copy, so stopping or
// unplugging at any moment is safe and a re-run resumes.

const (
	// maxTarFiles and maxTarChars bound one tar command (v1's adb
	// command-line budget is ~24,000 characters).
	maxTarFiles = 2000
	maxTarChars = 24000
	// tarAttempts: a stream that breaks mid-way is retried once with tar;
	// what is still missing after that goes to the pull lane.
	tarAttempts = 2
)

// compressibleExts are worth 'adb pull -z' (photos and videos are already
// compressed; text and databases shrink a lot).
var compressibleExts = map[string]bool{".txt": true, ".log": true, ".json": true, ".db": true, ".csv": true, ".xml": true, ".html": true}

// phoneLane is how a phone copy moves files.
type phoneLane struct {
	tar      bool
	why      string // why pull was used instead of tar ("" = asked for / tar used)
	compress string // 'adb pull -z' algorithm for big compressible files ("" = none)
}

func (l phoneLane) describe() string {
	switch {
	case l.tar:
		return "tar"
	case l.why != "":
		return "pull (" + l.why + ")"
	}
	return "pull"
}

// choosePhoneLane applies phone_copy_mode / --phone-mode and what the phone
// supports (probed once per phone).
func choosePhoneLane(ctx context.Context, p *Plan, opt Options) phoneLane {
	mode := strings.ToLower(strings.TrimSpace(opt.PhoneMode))
	if mode == "pull" {
		return phoneLane{} // exactly v1
	}
	caps, err := p.Phone.ADB.Probe(ctx, p.Phone.Serial)
	if err != nil {
		return phoneLane{why: "couldn't ask the phone what it supports"}
	}
	lane := phoneLane{compress: pickCompress(opt.PullCompress, caps.PullCompress)}
	if !caps.Tar {
		lane.why = "this phone has no tar that can stream"
		return lane
	}
	lane.tar = true
	return lane
}

// pickCompress turns phone_pull_compress into an algorithm the phone and adb
// both support, or "". "auto" stays off until a benchmark shows a gain.
func pickCompress(setting string, supported []string) string {
	s := strings.ToLower(strings.TrimSpace(setting))
	if s == "" || s == "auto" || s == "off" {
		return ""
	}
	for _, a := range supported {
		if a == s {
			return a
		}
	}
	return ""
}

// tarChunk is one tar command's worth of files.
type tarChunk struct {
	root  string           // the folder tar runs in ("cd <root>")
	rels  []string         // paths relative to root, in the order requested
	items map[string]*Item // by rel
}

// commonDir is the deepest phone folder holding every path in srcs.
func commonDir(srcs []string) string {
	if len(srcs) == 0 {
		return "/"
	}
	dir := path.Dir(srcs[0])
	for _, s := range srcs[1:] {
		for dir != "/" && !strings.HasPrefix(s, dir+"/") {
			dir = path.Dir(dir)
		}
	}
	return dir
}

// makeTarChunks cuts the tar lane's files into chunks of at most 2,000 files
// and 24,000 characters of arguments, in path order (files of one folder
// stay together).
func makeTarChunks(items []*Item) []tarChunk {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Src < items[b].Src })
	srcs := make([]string, len(items))
	for i, it := range items {
		srcs[i] = it.Src
	}
	root := commonDir(srcs)
	prefix := strings.TrimSuffix(root, "/") + "/"
	var chunks []tarChunk
	cur := tarChunk{root: root, items: map[string]*Item{}}
	chars := len(root) + 40
	for _, it := range items {
		rel := strings.TrimPrefix(it.Src, prefix)
		n := phone.QuotedLen(rel)
		if len(cur.rels) >= maxTarFiles || (chars+n > maxTarChars && len(cur.rels) > 0) {
			chunks = append(chunks, cur)
			cur = tarChunk{root: root, items: map[string]*Item{}}
			chars = len(root) + 40
		}
		cur.rels = append(cur.rels, rel)
		cur.items[rel] = it
		chars += n
	}
	if len(cur.rels) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// runPhoneTar copies a phone plan with the tar lane for small files and the
// pull lane (one worker while tar runs, two after) for everything else.
func runPhoneTar(ctx context.Context, p *Plan, opt Options, st *state, lane phoneLane) error {
	var tarItems, pullItems, compressItems []*Item
	for i := range p.Items {
		it := &p.Items[i]
		if it.Decision.Skipped() || it.Blocked != "" {
			continue
		}
		base := path.Base(it.Src)
		switch {
		case SafeName(base) != base:
			pullItems = append(pullItems, it) // a name Windows can't store: v1's SafeName path
		case it.Size >= bigFileSize:
			if lane.compress != "" && compressibleExts[strings.ToLower(path.Ext(base))] {
				compressItems = append(compressItems, it)
			} else {
				pullItems = append(pullItems, it)
			}
		default:
			tarItems = append(tarItems, it)
		}
	}

	queue := newWorkQueue[pullBatch]()
	queue.push(makeBatchesFor(pullItems)...)
	for _, b := range makeBatchesFor(compressItems) {
		b.compress = lane.compress
		queue.push(b)
	}

	var pullWG sync.WaitGroup
	startPull := func() {
		pullWG.Add(1)
		go func() {
			defer pullWG.Done()
			for {
				b, ok := queue.pop()
				if !ok {
					return
				}
				if ctx.Err() != nil {
					continue // drain without working
				}
				pullBatchInto(ctx, p, b, opt, st)
			}
		}()
	}
	startPull() // one pull worker while the tar lane runs

	streams := opt.TarStreams
	if streams <= 0 {
		streams = 1
	}
	chunks := make(chan tarChunk)
	var tarWG sync.WaitGroup
	for w := 0; w < streams; w++ {
		tarWG.Add(1)
		go func() {
			defer tarWG.Done()
			bufp := bufPool.Get().(*[]byte)
			defer bufPool.Put(bufp)
			for c := range chunks {
				copyTarChunk(ctx, p, c, *bufp, opt, st, queue)
			}
		}()
	}
dispatch:
	for _, c := range makeTarChunks(tarItems) {
		select {
		case chunks <- c:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(chunks)
	tarWG.Wait()

	for w := 1; w < pullWorkers; w++ {
		startPull() // the tar lane is done: the pull lane gets v1's two workers
	}
	queue.close()
	pullWG.Wait()
	return nil
}

// copyTarChunk streams one chunk, retries a broken stream once with tar,
// and hands what still didn't arrive to the pull lane.
func copyTarChunk(ctx context.Context, p *Plan, c tarChunk, buf []byte, opt Options, st *state, queue *workQueue[pullBatch]) {
	done := map[*Item]bool{}
	var streamErr error
	for attempt := 0; attempt < tarAttempts; attempt++ {
		var rels []string
		for _, rel := range c.rels {
			if !done[c.items[rel]] {
				rels = append(rels, rel)
			}
		}
		if len(rels) == 0 {
			return
		}
		streamErr = streamTar(ctx, p, c, rels, done, buf, opt, st)
		if ctx.Err() != nil {
			return
		}
		if streamErr == nil {
			break // a complete stream: whatever is missing isn't on the phone (any more)
		}
		// The stream broke mid-way (cable, phone locked...). Files already
		// renamed into place are done; the rest is asked for again.
	}
	var missing []*Item
	for _, rel := range c.rels {
		if it := c.items[rel]; !done[it] {
			missing = append(missing, it)
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, b := range makeBatchesFor(missing) {
		b.checkVanish = streamErr == nil // never delivered by a complete stream: deleted or unreadable
		queue.push(b)
	}
}

// errTrackReader remembers the first read error, so a failed write can be
// told apart from a broken stream.
type errTrackReader struct {
	r   io.Reader
	err error
}

func (e *errTrackReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// streamTar runs one tar stream for rels and writes every acceptable entry.
// It returns a non-nil error only when the STREAM failed (the files written
// before that are kept; a failure to write one file is that file's failure).
func streamTar(ctx context.Context, p *Plan, c tarChunk, rels []string, done map[*Item]bool, buf []byte, opt Options, st *state) error {
	tr := opt.Trace
	m := tr.Start()
	stream, wait, err := p.Phone.ADB.TarStream(ctx, p.Phone.Serial, c.root, rels)
	tr.End(trace.PhoneStream, m)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(rels))
	for _, r := range rels {
		wanted[r] = true
	}
	rd := tar.NewReader(stream)
	var streamErr error
	for {
		m := tr.Start()
		h, err := rd.Next()
		tr.End(trace.PhoneStream, m)
		if err == io.EOF {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		// Security: only a regular file whose cleaned name is one we asked
		// for in this stream is written. Anything else - an unrequested file,
		// a link, a device, "../evil", "/etc/x" - is skipped (archive/tar
		// drains its bytes on the next Next) and counted. A compromised or
		// buggy phone can't write outside the destination this way.
		name := phone.CleanTarName(h.Name)
		it := c.items[name]
		if !phone.SafeTarName(name) || !phone.IsRegular(h) || it == nil || !wanted[name] || done[it] {
			st.rejected.Add(1)
			continue
		}
		began := time.Now()
		er := &errTrackReader{r: rd}
		n, err := writeAtomic(ctx, filepath.Join(p.DstRoot, it.Rel), er, h.Size, h.ModTime.Unix(), buf, buffered, tr)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if er.err != nil {
				streamErr = er.err // broke mid-file: writeAtomic already removed the .ddxpart
				break
			}
			done[it] = true // the destination refused it: retrying via the phone won't help
			st.fail(it.Rel, err)
			continue
		}
		done[it] = true
		tr.FileDone(time.Since(began), n)
		st.written.Add(1)
		st.bytesDone.Add(n)
		st.filesDone.Add(1)
	}
	stream.Close()
	waitErr := wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if streamErr == nil && waitErr != nil {
		// tar exits non-zero when a requested file was missing; that is
		// handled by the caller (pull lane, then vanished). But if adb itself
		// failed and nothing arrived at all, treat it as a broken stream.
		var ee *phone.ExitError
		if !errors.As(waitErr, &ee) {
			return waitErr
		}
	}
	return streamErr
}
