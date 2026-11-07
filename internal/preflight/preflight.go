// Package preflight checks a copy before a single byte is written, and tells
// the user everything that will go wrong or be slow: not enough space (counted
// the way the destination really allocates it), files too big for FAT32,
// names the destination can't store, names that differ only in letter case,
// paths too long for Explorer, FAT folders that would overflow, a read-only
// card, a slow USB port - and how long the copy will take.
//
// What can be fixed is fixed automatically (renamed files are listed); what
// can't stops the copy with a clear message (exit code 3).
package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/winname"
)

// Severity says how serious an issue is.
type Severity int

const (
	// Info is shown and never stops anything.
	Info Severity = iota
	// Warn is shown; an interactive run asks "Continue? [y/N]" unless the
	// issue was already fixed or --yes was given.
	Warn
	// Block means the copy cannot start (exit code 3).
	Block
)

func (s Severity) String() string {
	switch s {
	case Warn:
		return "warn"
	case Block:
		return "block"
	}
	return "info"
}

// MarshalText makes severities readable in --json output.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads what MarshalText wrote (the agent parses its children's
// JSON results).
func (s *Severity) UnmarshalText(b []byte) error {
	switch string(b) {
	case "info":
		*s = Info
	case "warn":
		*s = Warn
	case "block":
		*s = Block
	default:
		return fmt.Errorf("unknown severity %q", b)
	}
	return nil
}

// maxPaths is how many affected paths an issue keeps (Count holds the total).
const maxPaths = 20

// Issue is one finding.
type Issue struct {
	Code     string   `json:"code"` // stable id, e.g. "case-collision"
	Severity Severity `json:"severity"`
	Paths    []string `json:"paths,omitempty"` // destination-relative, first 20
	Count    int      `json:"count,omitempty"`
	Message  string   `json:"message"`         // plain English, one sentence
	Hint     string   `json:"hint,omitempty"`  // what to do
	Fixed    string   `json:"fixed,omitempty"` // what DriveDex already did about it
}

// Report is the result of Run.
type Report struct {
	Issues   []Issue         `json:"issues"`
	Estimate Estimate        `json:"estimate"`
	Renames  []copier.Rename `json:"renames,omitempty"`
	// Space: bytes of the files to write, and what they take on the destination.
	FileBytes   int64 `json:"file_bytes"`
	OnDiskBytes int64 `json:"on_disk_bytes"`
}

// Blocked reports whether any issue stops the copy.
func (r Report) Blocked() bool {
	for _, is := range r.Issues {
		if is.Severity == Block {
			return true
		}
	}
	return false
}

// NeedsConfirmation reports whether there is a warning DriveDex did not fix
// itself (those are worth a y/N in an interactive run).
func (r Report) NeedsConfirmation() bool {
	for _, is := range r.Issues {
		if is.Severity == Warn && is.Fixed == "" {
			return true
		}
	}
	return false
}

// Has reports whether an issue with this code is in the report.
func (r Report) Has(code string) bool {
	for _, is := range r.Issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

// Get returns the issue with this code.
func (r Report) Get(code string) (Issue, bool) {
	for _, is := range r.Issues {
		if is.Code == code {
			return is, true
		}
	}
	return Issue{}, false
}

// Options are what Run needs besides the plan.
type Options struct {
	Names copier.NameOptions
	// FreeBytes returns the free space on the drive holding path (nil = not checked).
	FreeBytes func(path string) (uint64, error)
	// DryRun: nothing may be written, not even the write-probe file.
	DryRun bool
	// SlowPorts are USB drives involved in the copy that run below their speed.
	SlowPorts []SlowPort
	// SourceOffline names a source the plan was made from (an index) that is
	// not connected now.
	SourceOffline string
	// Verify adds the read-back to the estimate.
	Verify copier.VerifyMode
	// Src and Dst describe the two devices, for the time estimate.
	Src, Dst Device
	// Rates are learned transfer rates (from the run history); nil = defaults.
	Rates Rates
	// SyncRemovals is how many files a sync will move to the trash (they
	// keep using space there, so pre-flight counts them).
	TrashBytes int64
	// Extra are issues the caller found itself (the copy manifest, sync
	// guards); they are reported, and block, like the others.
	Extra []Issue
}

// SlowPort is one drive on a USB link slower than it could be.
type SlowPort struct {
	Drive  string // "E:"
	Advice string // what is in the way (volume.Advice)
	Tip    string
}

// Run checks the plan. It also applies the name fixes (renames) to the plan,
// so it must run before copier.Run.
func Run(p *copier.Plan, opt Options) Report {
	var rep Report
	add := func(is Issue) {
		if is.Count == 0 {
			is.Count = len(is.Paths)
		}
		if len(is.Paths) > maxPaths {
			is.Paths = is.Paths[:maxPaths]
		}
		rep.Issues = append(rep.Issues, is)
	}

	// --- names --------------------------------------------------------------
	nr := p.FixNames(opt.Names)
	rep.Renames = nr.Renames
	namesIssues(nr, opt.Names, add)
	for _, is := range opt.Extra {
		add(is)
	}

	counts := p.Counts()
	writing := counts.Copy + counts.Overwrite

	// --- links --------------------------------------------------------------
	if p.LinksSkipped > 0 {
		add(Issue{Code: "links-skipped", Severity: Info, Count: int(p.LinksSkipped),
			Message: fmt.Sprintf("%s %s in the source will be skipped (DriveDex never follows links).",
				ui.Thousands(p.LinksSkipped), plural(int(p.LinksSkipped), "link or junction", "links and junctions"))})
	}

	// --- daylight saving ----------------------------------------------------
	if n := p.DSTShifts(); n > 0 {
		add(Issue{Code: "dst-shift", Severity: Info, Count: int(n),
			Message: fmt.Sprintf("%s %s by exactly one hour (daylight-saving time on a FAT32 drive); treated as unchanged.",
				ui.Thousands(n), plural(int(n), "file differs", "files differ"))})
	}

	if opt.SourceOffline != "" {
		add(Issue{Code: "source-offline", Severity: Block,
			Message: opt.SourceOffline + " isn't connected, so its files can't be read now.",
			Hint:    "plug it in and run the command again, or queue the copy for when it is: drivedex queue add"})
	}

	if writing > 0 {
		// --- space ----------------------------------------------------------
		files, onDisk := p.SpaceNeeded()
		onDisk += opt.TrashBytes
		rep.FileBytes, rep.OnDiskBytes = files, onDisk
		if files > 0 && float64(onDisk) > float64(files)*1.10 && p.DstFS.ClusterBytes > 0 {
			add(Issue{Code: "cluster-slack", Severity: Info,
				Message: fmt.Sprintf("%s of files will use %s on this %s drive (%s clusters).",
					ui.HumanSize(files), ui.HumanSize(onDisk), fsName(p.DstFS), ui.HumanSize(p.DstFS.ClusterBytes))})
		}
		if opt.FreeBytes != nil {
			if free, err := opt.FreeBytes(fsinfo.NearestExisting(p.DstRoot)); err == nil && uint64(onDisk) > free {
				add(Issue{Code: "no-space", Severity: Block,
					Message: fmt.Sprintf("The copy needs %s on the destination but only %s is free.", ui.HumanSize(onDisk), ui.HumanSize(int64(free))),
					Hint:    fmt.Sprintf("free up at least %s, or choose another destination", ui.HumanSize(onDisk-int64(free)))})
			}
		}

		// --- FAT32 limits ---------------------------------------------------
		if p.DstFS.IsFAT() {
			var big []string
			for _, it := range p.Items {
				if !it.Decision.Skipped() && it.Size >= fat32MaxFile {
					big = append(big, it.Rel)
				}
			}
			if len(big) > 0 {
				add(Issue{Code: "fat32-4gib", Severity: Block, Paths: big,
					Message: fmt.Sprintf("The destination is FAT32, which can't store a file of 4 GB or more (%d %s).", len(big), plural(len(big), "file", "files")),
					Hint:    "copy to an NTFS or exFAT drive instead (or reformat this one as exFAT - back it up first)"})
			}
			if full := fatFullDirs(p); len(full) > 0 {
				add(Issue{Code: "fat-dir-full", Severity: Warn, Paths: full,
					Message: "A FAT32 folder can hold about 65,534 directory entries, and these folders would go over it (long names use several entries each).",
					Hint:    "use an exFAT or NTFS destination, or split the folder into several"})
			}
		}

		// --- long paths (Explorer's limit; other systems have none) ----------
		var long []string
		for _, it := range p.Items {
			if checkLongPaths && !it.Decision.Skipped() && winname.Units(filepath.Join(p.DstRoot, it.Rel)) > 259 {
				long = append(long, it.Rel)
			}
		}
		if len(long) > 0 {
			add(Issue{Code: "long-path", Severity: Warn, Paths: long,
				Message: fmt.Sprintf("%d %s will have a full path longer than 259 characters; DriveDex writes them fine, but Explorer and many programs can't open them.", len(long), plural(len(long), "file", "files")),
				Hint:    "choose a destination folder closer to the drive's root"})
		}

		// --- read-only --------------------------------------------------------
		if why := readOnly(p, opt.DryRun); why != "" {
			add(Issue{Code: "read-only", Severity: Block, Message: why,
				Hint: "on an SD card, slide the small lock switch on its side to unlocked; otherwise check you may write to this drive"})
		}
	}

	// --- slow USB ports ---------------------------------------------------------
	for _, sp := range opt.SlowPorts {
		add(Issue{Code: "slow-port", Severity: Info, Message: sp.Drive + " " + sp.Advice, Hint: sp.Tip})
	}

	// --- time estimate ----------------------------------------------------------
	var fileCount int64
	for _, it := range p.Items {
		if !it.Decision.Skipped() {
			fileCount++
		}
	}
	rep.Estimate = EstimateCopy(opt.Src, opt.Dst, counts.WriteBytes, fileCount, opt.Verify, opt.Rates)

	sort.SliceStable(rep.Issues, func(a, b int) bool { return rep.Issues[a].Severity > rep.Issues[b].Severity })
	return rep
}

// fat32MaxFile is the largest file FAT32 can store (4 GiB minus one byte).
const fat32MaxFile = 4 << 30

// checkLongPaths turns on the 259-character warning: it is Explorer's limit,
// so it only means something on Windows.
var checkLongPaths = runtime.GOOS == "windows"

// fatDirLimit is how many 32-byte directory entries one FAT folder can have.
const fatDirLimit = 65534

func fsName(i fsinfo.Info) string {
	if i.FileSystem == "" {
		return "destination"
	}
	return i.FileSystem
}

// namesIssues turns what FixNames did into issues.
func namesIssues(nr copier.NameReport, opt copier.NameOptions, add func(Issue)) {
	byReason := map[string][]string{}
	for _, r := range nr.Renames {
		byReason[r.Reason] = append(byReason[r.Reason], r.From+" → "+filepath.Base(r.To))
	}
	if len(nr.CaseCollisions) > 0 {
		is := Issue{Code: "case-collision", Severity: Warn, Count: len(nr.CaseCollisions),
			Message: fmt.Sprintf("%d %s only in letter case from another, which is the same name on this destination.",
				len(nr.CaseCollisions), plural(len(nr.CaseCollisions), "name differs", "names differ"))}
		switch opt.OnCaseCollision {
		case copier.OnCaseSkip:
			is.Paths, is.Fixed = nr.CaseCollisions, "skipped (not copied)"
		case copier.OnCaseFail:
			is.Paths, is.Severity = nr.CaseCollisions, Block
			is.Hint = "use --on-case-collision rename (the default) or skip"
		default:
			is.Paths, is.Fixed = byReason["case-collision"], "renamed with \" (case 2)\""
		}
		add(is)
	}
	if len(nr.BadNames) > 0 {
		is := Issue{Code: "bad-name", Severity: Warn, Count: len(nr.BadNames),
			Message: fmt.Sprintf("%d %s characters or a form Windows can't store (such as : ? * or a trailing dot).",
				len(nr.BadNames), plural(len(nr.BadNames), "name has", "names have"))}
		switch opt.OnBadName {
		case copier.OnBadSkip:
			is.Paths, is.Fixed = nr.BadNames, "skipped (not copied)"
		case copier.OnBadFail:
			is.Paths, is.Severity = nr.BadNames, Block
			is.Hint = "use --on-bad-name replace (the default) or skip"
		default:
			is.Paths, is.Fixed = byReason["bad-name"], "replaced with look-alike characters"
		}
		add(is)
	}
	if len(nr.TooLong) > 0 {
		is := Issue{Code: "name-too-long", Severity: Block, Paths: nr.TooLong, Count: len(nr.TooLong),
			Message: fmt.Sprintf("%d %s longer than the 255 characters Windows allows.", len(nr.TooLong), plural(len(nr.TooLong), "name is", "names are")),
			Hint:    "use --on-bad-name replace (the default) to shorten them, or skip"}
		switch opt.OnBadName {
		case copier.OnBadSkip:
			is.Severity, is.Fixed, is.Hint = Warn, "skipped (not copied)", ""
		case copier.OnBadFail:
		default:
			is.Severity, is.Paths, is.Fixed, is.Hint = Warn, byReason["name-too-long"], "shortened, keeping the extension and a ~hash mark", ""
		}
		add(is)
	}
}

// readOnly says why the destination can't be written, or "".
func readOnly(p *copier.Plan, dryRun bool) string {
	if p.DstFS.ReadOnly {
		return "The destination drive is read-only."
	}
	if dryRun {
		return "" // a dry run writes nothing, not even a probe
	}
	dir := fsinfo.NearestExisting(p.DstRoot)
	f, err := os.CreateTemp(dir, ".ddxprobe-*")
	if err != nil {
		if os.IsPermission(err) || strings.Contains(strings.ToLower(err.Error()), "write-protected") || strings.Contains(strings.ToLower(err.Error()), "write protected") {
			return "DriveDex can't create files on the destination (" + dir + "): it is write-protected."
		}
		return "" // some other problem: the copy itself will report it precisely
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return ""
}

// fatFullDirs lists destination folders that would exceed FAT's limit of
// directory entries. Each file takes one entry plus one per 13 characters of
// its long name.
func fatFullDirs(p *copier.Plan) []string {
	entries := map[string]int{}
	seen := map[string]bool{}
	count := func(dir, name string) {
		key := dir + "\x00" + winname.Fold(name)
		if seen[key] {
			return
		}
		seen[key] = true
		entries[dir] += 1 + (winname.Units(name)+12)/13
	}
	for _, it := range p.Items {
		if it.Decision.Skipped() {
			continue
		}
		count(filepath.Dir(it.Rel), filepath.Base(it.Rel))
	}
	for _, d := range p.Dirs {
		count(filepath.Dir(d), filepath.Base(d))
	}
	var full []string
	for dir, n := range entries {
		if n < fatDirLimit/2 {
			continue // can't be near the limit, not worth listing the folder
		}
		if names, err := os.ReadDir(filepath.Join(p.DstRoot, dir)); err == nil {
			for _, e := range names {
				count(dir, e.Name())
			}
		}
		if entries[dir] > fatDirLimit {
			full = append(full, dir)
		}
	}
	sort.Strings(full)
	return full
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
