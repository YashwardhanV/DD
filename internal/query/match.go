package query

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// Matcher applies a Query to index entries. It implements index.Matcher, so
// the parallel, ranked search in package index runs it unchanged.
//
// PERF: the checks run from cheapest to most expensive and stop at the first
// failure - type, extension, name words, then (only for entries that survived)
// the full path, regular expressions, size and date.
type Matcher struct {
	q   *Query
	env index.Env
	// HideHidden drops entries marked hidden or system (config show_hidden = false).
	HideHidden bool

	scorer *index.TermMatcher
	noMeta atomic.Int64
}

// Matcher builds a matcher for the query. env says which drives are connected
// (needed for drive: filters).
func (q *Query) Matcher(env index.Env) *Matcher {
	return &Matcher{q: q, env: env, scorer: index.NewTermMatcher(strings.Join(q.Terms, " "))}
}

// SkippedNoMeta is how many entries were passed over because the query
// filters on size/date but the index doesn't know them (NTFS turbo mode
// stores names only). The CLI uses it to print a helpful tip.
func (m *Matcher) SkippedNoMeta() int64 { return m.noMeta.Load() }

// Applies implements index.Matcher: drive: filters decide which indexes are searched at all.
func (m *Matcher) Applies(h *index.Header) bool {
	if len(m.q.Drives) == 0 {
		return true
	}
	for _, d := range m.q.Drives {
		switch {
		case d.Phone:
			// Most portable devices are phones in "File transfer" mode.
			if h.SourceKind == index.KindPhone || h.SourceKind == index.KindDevice {
				return true
			}
		case d.Device:
			if h.SourceKind == index.KindDevice {
				return true
			}
		case d.Letter != "":
			if h.SourceKind == index.KindVolume && strings.EqualFold(m.letterOf(h), d.Letter) {
				return true
			}
		default:
			if strings.EqualFold(strings.TrimSpace(h.Label), d.Name) || strings.EqualFold(h.SourceID, d.Name) {
				return true
			}
		}
	}
	return false
}

// letterOf is the drive letter of a volume index: where it is plugged in now,
// or else where it was last seen.
func (m *Matcher) letterOf(h *index.Header) string {
	root, mounted := m.env.Mounts[h.SourceID]
	if !mounted {
		root = h.LastMount
	}
	root = strings.TrimRight(root, `\/`)
	if len(root) >= 2 && root[1] == ':' {
		return strings.ToUpper(root[:2])
	}
	return ""
}

// Match implements index.Matcher.
func (m *Matcher) Match(ix *index.Index, slot int) bool {
	q := m.q
	flags := ix.Flags[slot]
	isDir := flags&index.FlagDir != 0

	// 1. the cheapest tests
	switch q.Type {
	case FilesOnly:
		if isDir {
			return false
		}
	case DirsOnly:
		if !isDir {
			return false
		}
	}
	if m.HideHidden && flags&(index.FlagHidden|index.FlagSystem) != 0 {
		return false
	}
	name := ix.Lower[slot]
	if len(q.Exts) > 0 && (isDir || !hasExt(name, q.Exts)) {
		return false
	}
	for _, t := range q.Terms {
		if !strings.Contains(name, t) {
			return false
		}
	}
	for _, t := range q.Exclude {
		if strings.Contains(name, t) {
			return false
		}
	}

	// 2. tests that cost more (they only run for entries that got this far)
	if len(q.PathTerms) > 0 {
		path := strings.ToLower(ix.RelPath(slot))
		for _, t := range q.PathTerms {
			if !strings.Contains(path, t) {
				return false
			}
		}
	}
	for _, re := range q.Regexes {
		if !re.MatchString(ix.Names[slot]) {
			return false
		}
	}
	if q.Size != nil {
		if flags&index.FlagNoMeta != 0 || (!isDir && ix.Sizes[slot] < 0) {
			m.noMeta.Add(1)
			return false
		}
		size := ix.Sizes[slot]
		if isDir {
			size = 0
		}
		if !q.Size.Contains(size) {
			return false
		}
	}
	if q.Modified != nil {
		if flags&index.FlagNoMeta != 0 || ix.MTimes[slot] == 0 {
			m.noMeta.Add(1)
			return false
		}
		if !q.Modified.Contains(ix.MTimes[slot]) {
			return false
		}
	}
	return true
}

// Score implements index.Matcher (same ranking as plain word searches, based on
// the plain words; a query of only filters ranks everything equally).
func (m *Matcher) Score(ix *index.Index, slot int) int { return m.scorer.Score(ix, slot) }

// hasExt reports whether the lowercase file name ends in one of the
// extensions. Multi-part extensions work too: ext:tar.gz matches
// "backup.tar.gz".
func hasExt(lowerName string, exts []string) bool {
	for _, e := range exts {
		if len(lowerName) >= len(e)+1 && strings.HasSuffix(lowerName, e) && lowerName[len(lowerName)-len(e)-1] == '.' {
			return true
		}
	}
	return false
}

// Compile-time proof that Matcher satisfies the interface.
var _ index.Matcher = (*Matcher)(nil)

// Now is the clock used by callers that parse queries (a variable for tests).
var Now = time.Now
