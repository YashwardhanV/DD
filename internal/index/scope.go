package index

import (
	"path/filepath"
	"strings"
)

// FindFolder returns the ID of the folder at rel (a path with `\` separators,
// relative to the source root; "" means the root itself, which has ID 0).
// Names are compared case-insensitively, as Windows does.
func (ix *Index) FindFolder(rel string) (id uint64, ok bool) {
	rel = strings.Trim(rel, `\/`)
	if rel == "" {
		return 0, true
	}
	cur := uint64(0)
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '\\' || r == '/' }) {
		found := false
		for slot := range ix.IDs {
			if ix.Parents[slot] == cur && ix.Flags[slot]&(FlagDir|FlagDeleted) == FlagDir && strings.EqualFold(ix.Names[slot], part) {
				cur, found = ix.IDs[slot], true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return cur, true
}

// maxDepth guards the parent walk against corrupt (cyclic) data.
const maxUnderDepth = 4096

// Under reports whether the entry in slot lies somewhere inside the folder
// with the given ID (at any depth). folderID 0 means "anywhere in the source".
func (ix *Index) Under(slot int, folderID uint64) bool {
	if folderID == 0 {
		return true
	}
	p := ix.Parents[slot]
	for depth := 0; p != 0 && depth < maxUnderDepth; depth++ {
		if p == folderID {
			return true
		}
		s, ok := ix.byID[p]
		if !ok {
			return false
		}
		p = ix.Parents[s]
	}
	return false
}

// ScopeMatcher restricts another matcher to one folder of one source - this is
// what `drivedex tui --scope E:\Photos` uses.
type ScopeMatcher struct {
	Inner    Matcher
	SourceID string
	FolderID uint64 // 0 = the whole source
}

// Applies implements Matcher: only the scoped source is searched at all.
func (s *ScopeMatcher) Applies(h *Header) bool {
	return h.SourceID == s.SourceID && s.Inner.Applies(h)
}

// Match implements Matcher. The cheap name test runs first; the (slower)
// ancestor walk only happens for entries whose name already matched.
func (s *ScopeMatcher) Match(ix *Index, slot int) bool {
	return s.Inner.Match(ix, slot) && ix.Under(slot, s.FolderID)
}

// Score implements Matcher.
func (s *ScopeMatcher) Score(ix *Index, slot int) int { return s.Inner.Score(ix, slot) }

// FolderScope describes where a folder on this PC lives in the saved indexes.
type FolderScope struct {
	Index    *Index
	FolderID uint64
	Rel      string // folder path relative to the source root, with `\`
}

// LocateScope finds which saved index covers the local folder abs, and the
// folder's entry inside it. Volumes are located through their current mount
// (env.Mounts); folder sources through their RootPath. When several indexes
// contain the folder the one with the longest (most specific) root wins.
// ok is false if no index covers it, or the folder isn't in the index.
func LocateScope(indexes []*Index, env Env, abs string) (FolderScope, bool) {
	best := -1
	bestRel := ""
	bestLen := -1
	for i, ix := range indexes {
		h := &ix.Header
		var root string
		switch h.SourceKind {
		case KindVolume:
			r, mounted := env.Mounts[h.SourceID]
			if !mounted {
				continue
			}
			root = r
		case KindFolder:
			root = h.RootPath
		default:
			continue
		}
		rel, within := relativeTo(root, abs)
		if within && len(root) > bestLen {
			best, bestRel, bestLen = i, rel, len(root)
		}
	}
	if best < 0 {
		return FolderScope{}, false
	}
	id, ok := indexes[best].FindFolder(bestRel)
	if !ok {
		return FolderScope{}, false
	}
	return FolderScope{Index: indexes[best], FolderID: id, Rel: bestRel}, true
}

// relativeTo returns abs relative to root with `\` separators, if abs is root
// or inside it (case-insensitively, like Windows paths).
func relativeTo(root, abs string) (string, bool) {
	r := strings.ToLower(filepath.Clean(root))
	a := filepath.Clean(abs)
	la := strings.ToLower(a)
	if la == r {
		return "", true
	}
	sep := string(filepath.Separator)
	prefix := r
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if !strings.HasPrefix(la, prefix) {
		return "", false
	}
	return strings.ReplaceAll(a[len(prefix):], sep, `\`), true
}

// SourceFilter restricts another matcher to a single index (`--drive E:`,
// `--source vol-1A2B3C4D`).
type SourceFilter struct {
	Inner    Matcher
	SourceID string
}

// Applies implements Matcher.
func (s *SourceFilter) Applies(h *Header) bool { return h.SourceID == s.SourceID && s.Inner.Applies(h) }

// Match implements Matcher.
func (s *SourceFilter) Match(ix *Index, slot int) bool { return s.Inner.Match(ix, slot) }

// Score implements Matcher.
func (s *SourceFilter) Score(ix *Index, slot int) int { return s.Inner.Score(ix, slot) }
