package copier

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/winname"
)

// Choices for names the destination can't take as they are.
const (
	OnCaseRename = "rename" // a.jpg and A.jpg: the second becomes "A (case 2).jpg" (default)
	OnCaseSkip   = "skip"   // the second one is not copied
	OnCaseFail   = "fail"   // the copy doesn't start

	OnBadReplace = "replace" // notes:1.txt becomes notes：1.txt (default)
	OnBadSkip    = "skip"
	OnBadFail    = "fail"
)

// NameOptions say what to do with names the destination can't hold.
type NameOptions struct {
	OnCaseCollision string // rename | skip | fail
	OnBadName       string // replace | skip | fail
	// CaseSensitiveDest: folders the copy creates get NTFS per-folder case
	// sensitivity, so a.jpg and A.jpg can live side by side there.
	CaseSensitiveDest bool
}

// Rename is one planned name change.
type Rename struct {
	From, To string // destination-relative paths (OS separators)
	Reason   string // "bad-name" | "case-collision" | "name-too-long"
}

// NameReport is what FixNames found and did.
type NameReport struct {
	Renames []Rename
	// Problem paths per kind (destination-relative, original names) that were
	// skipped or, in fail mode, block the copy.
	CaseCollisions []string
	BadNames       []string
	TooLong        []string
	// Skipped files (skip mode) and whether any fail-mode rule was hit.
	SkippedFiles int
	Fail         bool
}

// node is one destination path in the name tree.
type node struct {
	orig     string // original name
	final    string // name after fixing
	parent   *node
	children []*node
	isDir    bool
	items    []int // plan item indexes of files with this path
	skip     bool
}

func (n *node) path(final bool) string {
	var parts []string
	for c := n; c != nil && c.parent != nil; c = c.parent {
		if final {
			parts = append(parts, c.final)
		} else {
			parts = append(parts, c.orig)
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	// Not filepath.Join: it cleans the result, and on Windows a phone file
	// named "c:x.jpg" at the start would then read as a drive letter.
	return strings.Join(parts, string(filepath.Separator))
}

// splitPath splits a destination-relative path into its names, without
// interpreting any of them (a name may contain ':' or end with a dot).
func splitPath(rel string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if p != "." {
			out = append(out, p)
		}
	}
	return out
}

// listDir returns the names in a destination folder ("" error = it exists).
// A variable so tests can fake an existing destination.
var listDir = func(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// caseSensitiveDir reports NTFS per-folder case sensitivity (a variable for tests).
var caseSensitiveDir = fsinfo.CaseSensitiveDir

// FixNames makes every destination path one the destination can store:
//
//   - names Windows can't store (phones and Linux allow them) are replaced
//     by look-alikes (winname.Fix), skipped, or block the copy;
//   - on a case-insensitive destination, two paths that differ only in
//     letter case (a.jpg and A.jpg from a phone), or a planned path and an
//     existing destination entry that differ only in case, would be the same
//     file: the second (in name order) becomes "A (case 2).jpg", is skipped,
//     or blocks the copy.
//
// Every new name is checked again, so renaming can never create a new clash.
// Folders and files share one namespace. Folder names are fixed once, and
// every file inside follows. Item.Job.SrcRel keeps the original path.
func (p *Plan) FixNames(opt NameOptions) NameReport {
	var rep NameReport
	p.NamesFixed = true
	windowsNames := p.DstFS.IsWindowsFS()
	root := &node{isDir: true}
	byPath := map[string]*node{"": root}

	// get finds or creates the node for a path given as its names; the map
	// key joins them with NUL, which no name can contain.
	get := func(parts []string, isDir bool) *node {
		cur := root
		for i := range parts {
			key := strings.Join(parts[:i+1], "\x00")
			n, ok := byPath[key]
			if !ok {
				n = &node{orig: parts[i], parent: cur, isDir: true}
				cur.children = append(cur.children, n)
				byPath[key] = n
			}
			cur = n
		}
		if cur != root {
			cur.isDir = isDir || len(cur.children) > 0
		}
		return cur
	}
	for _, d := range p.Dirs {
		if parts := splitPath(d); len(parts) > 0 {
			get(parts, true)
		}
	}
	for i := range p.Items {
		parts := splitPath(p.Items[i].Rel)
		if len(parts) == 0 {
			continue
		}
		n := get(parts, false)
		n.items = append(n.items, i)
	}

	changed := false
	var visit func(dir *node)
	visit = func(dir *node) {
		finalDir := dir.path(true)
		// Names already in this destination folder (if it exists).
		existing := map[string]string{} // folded -> exact
		exists := false
		if names, ok := p.dstNames[finalDir]; ok {
			exists = true
			for _, nm := range names {
				existing[winname.Fold(nm)] = nm
			}
		} else if names, err := listDir(filepath.Join(p.DstRoot, finalDir)); err == nil {
			exists = true
			for _, nm := range names {
				existing[winname.Fold(nm)] = nm
			}
		}
		sensitive := p.DstFS.CaseSensitive
		if !sensitive && !exists && opt.CaseSensitiveDest && p.DstFS.IsNTFS() {
			sensitive = true // the copy will create it case-sensitive
		}
		checkedFolder := false

		// Names that are already valid go first, so a name that has to be
		// fixed can never take the place of one that didn't (notes. becomes
		// notes_, which must not push an existing notes_ aside). Within each
		// group: name order, so the result doesn't depend on the source's
		// listing order.
		sort.Slice(dir.children, func(a, b int) bool {
			ca, cb := dir.children[a], dir.children[b]
			if va, vb := !windowsNames || winname.Valid(ca.orig), !windowsNames || winname.Valid(cb.orig); va != vb {
				return va
			}
			return ca.orig < cb.orig
		})
		used := map[string]bool{}
		clash := func(name string) bool {
			if sensitive {
				return false
			}
			f := winname.Fold(name)
			if used[f] {
				return true
			}
			ex, ok := existing[f]
			return ok && ex != name
		}
		for _, c := range dir.children {
			c.final = c.orig
			if windowsNames {
				if fixed, bad := winname.Fix(c.orig); bad {
					problem := winname.Check(c.orig)
					list, reason := &rep.BadNames, "bad-name"
					if problem == winname.TooLong {
						list, reason = &rep.TooLong, "name-too-long"
					}
					*list = append(*list, joinRel(dir.path(false), c.orig))
					switch opt.OnBadName {
					case OnBadSkip:
						c.skip = true
						continue
					case OnBadFail:
						rep.Fail = true
						continue
					default:
						c.final = fixed
						rep.Renames = append(rep.Renames, Rename{From: joinRel(dir.path(false), c.orig), To: joinRel(finalDir, fixed), Reason: reason})
					}
				}
			}
			if clash(c.final) && !checkedFolder && exists {
				// Only now is it worth asking whether this folder has NTFS's
				// per-folder case sensitivity (folders shared with WSL do).
				checkedFolder = true
				if caseSensitiveDir(filepath.Join(p.DstRoot, finalDir)) {
					sensitive = true
				}
			}
			if clash(c.final) {
				rep.CaseCollisions = append(rep.CaseCollisions, joinRel(dir.path(false), c.orig))
				switch opt.OnCaseCollision {
				case OnCaseSkip:
					c.skip = true
					continue
				case OnCaseFail:
					rep.Fail = true
					used[winname.Fold(c.final)] = true
					continue
				default:
					base := c.final
					for k := 2; ; k++ {
						cand := winname.CaseVariant(base, k)
						if winname.Units(cand) > winname.MaxUnits {
							cand, _ = winname.Fix(cand) // shortened, still distinct by its hash mark
						}
						if !clash(cand) {
							c.final = cand
							break
						}
					}
					rep.Renames = append(rep.Renames, Rename{From: joinRel(dir.path(false), c.orig), To: joinRel(finalDir, c.final), Reason: "case-collision"})
				}
			}
			if c.final != c.orig {
				changed = true
			}
			used[winname.Fold(c.final)] = true
		}
		for _, c := range dir.children {
			if !c.skip && c.isDir {
				visit(c)
			}
		}
	}
	visit(root)

	// Count skipped files (a skipped folder skips everything inside it).
	var markSkipped func(n *node)
	markSkipped = func(n *node) {
		for _, i := range n.items {
			p.Items[i].Decision = skipName
		}
		for _, c := range n.children {
			markSkipped(c)
		}
	}
	anySkip := false
	for _, n := range byPath {
		if n.skip {
			anySkip = true
			markSkipped(n)
		}
	}
	if !changed && !anySkip {
		return rep
	}

	// Apply the new names.
	var keep []Item
	for _, n := range byPath {
		if n == root || len(n.items) == 0 {
			continue
		}
		final := n.path(true)
		orig := n.path(false)
		for _, i := range n.items {
			if p.Items[i].Decision == skipName {
				continue
			}
			if final != orig {
				p.Items[i].SrcRel = orig
				p.Items[i].Rel = final
				p.Items[i].Renamed = true
			}
		}
	}
	for _, it := range p.Items {
		if it.Decision == skipName {
			rep.SkippedFiles++
			continue
		}
		keep = append(keep, it)
	}
	p.Items = keep
	var dirs []string
	for _, d := range p.Dirs {
		n := byPath[strings.Join(splitPath(d), "\x00")]
		if n == nil || n == root {
			dirs = append(dirs, d)
			continue
		}
		if skippedUnder(n) {
			continue
		}
		dirs = append(dirs, n.path(true))
	}
	sort.Strings(dirs)
	p.Dirs = dirs
	// Renamed files may now exist at the destination under their new name:
	// decide again for those.
	for i := range p.Items {
		it := &p.Items[i]
		if !it.Renamed {
			continue
		}
		st, err := os.Lstat(filepath.Join(p.DstRoot, it.Rel))
		if err != nil {
			st = nil
		}
		dec := decide(p.OnConflict, it.Job, st, p.SrcFS, p.DstFS)
		it.Decision, it.Blocked, it.DSTShift = dec.d, dec.blocked, dec.shift
	}
	p.countNewDirs()
	return rep
}

// skipName marks an item dropped by FixNames (never left in a plan).
const skipName Decision = 255

func skippedUnder(n *node) bool {
	for c := n; c != nil; c = c.parent {
		if c.skip {
			return true
		}
	}
	return false
}

// joinRel joins a relative folder path and a name without cleaning them.
func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + string(filepath.Separator) + name
}
