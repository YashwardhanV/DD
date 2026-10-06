package index

import (
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Matcher decides which entries match a search and how well. The basic
// TermMatcher below is what `drivedex search resume pdf` uses; stage 11's
// richer query language (ext:, size:, ...) plugs in by implementing the same
// interface, so the parallel search machinery here never has to change.
type Matcher interface {
	// Applies lets a matcher skip a whole index cheaply (for example when the
	// user asked for one drive only).
	Applies(h *Header) bool
	// Match reports whether the entry in slot matches. It is called from many
	// goroutines at once and must not modify anything.
	Match(ix *Index, slot int) bool
	// Score ranks a matching entry; lower is better. See TermMatcher.Score.
	Score(ix *Index, slot int) int
}

// SearchOptions tunes a search.
type SearchOptions struct {
	// Limit is how many top results to return. Zero or negative means all.
	Limit int
}

// Hit is one search result: where it lives and how well it matched.
type Hit struct {
	Index int // position in the slice of indexes passed to Search
	Slot  int // slot inside that index
	Score int
}

// SearchResult is what Search returns.
type SearchResult struct {
	Hits     []Hit // best first, at most Limit
	Total    int   // number of matches in all (not just the returned ones)
	Searched int   // number of entries examined
	Indexes  int   // number of indexes searched
}

// minChunk is the smallest slice of an index worth giving its own goroutine.
const minChunk = 20000

// Search runs m over all indexes.
//
// PERF: every index is split into chunks of slots, one goroutine per chunk
// (up to the number of CPUs). Each goroutine counts its matches and keeps only
// its own best Limit candidates in a small heap; the heaps are merged at the
// end. That way a query that matches 900,000 names never sorts 900,000 hits.
func Search(indexes []*Index, m Matcher, opt SearchOptions) SearchResult {
	var res SearchResult

	type job struct{ ix, start, end int }
	var jobs []job
	for i, ix := range indexes {
		if !m.Applies(&ix.Header) {
			continue
		}
		res.Indexes++
		res.Searched += ix.LiveLen()
		n := ix.Len()
		chunks := runtime.NumCPU()
		if max := n/minChunk + 1; chunks > max {
			chunks = max
		}
		size := (n + chunks - 1) / chunks
		for start := 0; start < n; start += size {
			end := start + size
			if end > n {
				end = n
			}
			jobs = append(jobs, job{i, start, end})
		}
	}

	type chunkResult struct {
		hits  []candidate
		total int
	}
	results := make([]chunkResult, len(jobs))
	var wg sync.WaitGroup
	for ji, j := range jobs {
		wg.Add(1)
		go func(ji int, j job) {
			defer wg.Done()
			ix := indexes[j.ix]
			top := newTopK(opt.Limit, indexes)
			total := 0
			for slot := j.start; slot < j.end; slot++ {
				if ix.Flags[slot]&FlagDeleted != 0 {
					continue
				}
				if !m.Match(ix, slot) {
					continue
				}
				total++
				top.offer(candidate{
					ix:      int32(j.ix),
					slot:    int32(slot),
					score:   int32(m.Score(ix, slot)),
					nameLen: int32(len(ix.Names[slot])),
					isDir:   ix.Flags[slot]&FlagDir != 0,
				})
			}
			results[ji] = chunkResult{hits: top.items, total: total}
		}(ji, j)
	}
	wg.Wait()

	// Merge: pool every chunk's candidates, sort once, keep the best Limit.
	var pool []candidate
	for _, r := range results {
		res.Total += r.total
		pool = append(pool, r.hits...)
	}
	rs := newResolver(indexes)
	sort.Slice(pool, func(a, b int) bool { return rs.less(pool[a], pool[b]) })
	if opt.Limit > 0 && len(pool) > opt.Limit {
		pool = pool[:opt.Limit]
	}
	res.Hits = make([]Hit, len(pool))
	for i, c := range pool {
		res.Hits[i] = Hit{Index: int(c.ix), Slot: int(c.slot), Score: int(c.score)}
	}
	return res
}

// candidate is a matching entry with everything needed to rank it.
type candidate struct {
	ix, slot, score, nameLen int32
	isDir                    bool
}

// resolver compares candidates. Ranking order (all ascending):
//  1. score
//  2. shorter name
//  3. files before folders
//  4. path alphabetically (the only step that has to build paths, so it is
//     reached only for real ties)
//  5. position, so equal entries always come out in the same order
//
// A resolver is not safe for concurrent use; each goroutine makes its own.
type resolver struct {
	indexes []*Index
	caches  []map[uint64]string // per-index folder path cache
}

func newResolver(indexes []*Index) *resolver {
	return &resolver{indexes: indexes, caches: make([]map[uint64]string, len(indexes))}
}

func (r *resolver) path(c candidate) string {
	if r.caches[c.ix] == nil {
		r.caches[c.ix] = make(map[uint64]string)
	}
	return strings.ToLower(r.indexes[c.ix].RelPathCached(int(c.slot), r.caches[c.ix]))
}

func (r *resolver) less(a, b candidate) bool {
	switch {
	case a.score != b.score:
		return a.score < b.score
	case a.nameLen != b.nameLen:
		return a.nameLen < b.nameLen
	case a.isDir != b.isDir:
		return !a.isDir
	}
	if pa, pb := r.path(a), r.path(b); pa != pb {
		return pa < pb
	}
	if a.ix != b.ix {
		return a.ix < b.ix
	}
	return a.slot < b.slot
}

// topK keeps the best k candidates seen so far using a binary max-heap whose
// root is the WORST kept candidate, so a new one only has to beat the root.
// k <= 0 keeps everything.
type topK struct {
	k     int
	items []candidate
	rs    *resolver
}

func newTopK(k int, indexes []*Index) *topK { return &topK{k: k, rs: newResolver(indexes)} }

func (t *topK) offer(c candidate) {
	if t.k <= 0 {
		t.items = append(t.items, c)
		return
	}
	if len(t.items) < t.k {
		t.items = append(t.items, c)
		t.siftUp(len(t.items) - 1)
		return
	}
	if t.rs.less(c, t.items[0]) {
		t.items[0] = c
		t.siftDown(0)
	}
}

// worse reports whether item i should sit above item j in the max-heap.
func (t *topK) worse(i, j int) bool { return t.rs.less(t.items[j], t.items[i]) }

func (t *topK) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !t.worse(i, p) {
			return
		}
		t.items[i], t.items[p] = t.items[p], t.items[i]
		i = p
	}
}

func (t *topK) siftDown(i int) {
	n := len(t.items)
	for {
		l, r, w := 2*i+1, 2*i+2, i
		if l < n && t.worse(l, w) {
			w = l
		}
		if r < n && t.worse(r, w) {
			w = r
		}
		if w == i {
			return
		}
		t.items[i], t.items[w] = t.items[w], t.items[i]
		i = w
	}
}

// ---------------------------------------------------------------------------
// Basic term matching
// ---------------------------------------------------------------------------

// ParseTerms splits a query into lowercase terms. Whitespace separates terms;
// "double quoted phrases" stay together as one term.
func ParseTerms(q string) []string {
	var terms []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote {
				flush() // closing quote ends the phrase
			}
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

// TermMatcher is the simple matcher: an entry matches when EVERY term is a
// substring of its lowercase name.
type TermMatcher struct {
	Terms []string // already lowercase
	// SourceID, if set, restricts the search to that one index.
	SourceID string
	// OnlyFiles / OnlyDirs restrict results to one kind of entry.
	OnlyFiles, OnlyDirs bool

	whole string // the terms joined, for the "exact name" ranking
}

// NewTermMatcher builds a TermMatcher from a query string.
func NewTermMatcher(query string) *TermMatcher {
	t := ParseTerms(query)
	return &TermMatcher{Terms: t, whole: strings.Join(t, " ")}
}

// Applies implements Matcher.
func (m *TermMatcher) Applies(h *Header) bool { return m.SourceID == "" || h.SourceID == m.SourceID }

// Match implements Matcher.
func (m *TermMatcher) Match(ix *Index, slot int) bool {
	if m.OnlyFiles || m.OnlyDirs {
		isDir := ix.Flags[slot]&FlagDir != 0
		if (m.OnlyFiles && isDir) || (m.OnlyDirs && !isDir) {
			return false
		}
	}
	name := ix.Lower[slot]
	for _, t := range m.Terms {
		if !strings.Contains(name, t) {
			return false
		}
	}
	return true
}

// Score implements Matcher (lower is better):
//
//	0  the name is exactly the whole query
//	1  the name starts with the first term
//	2  the first term starts at a word boundary (after space _ - . ( )
//	3  anything else
func (m *TermMatcher) Score(ix *Index, slot int) int {
	name := ix.Lower[slot]
	if len(m.Terms) == 0 {
		return 3
	}
	if name == m.whole {
		return 0
	}
	first := m.Terms[0]
	if strings.HasPrefix(name, first) {
		return 1
	}
	for off := 0; ; {
		i := strings.Index(name[off:], first)
		if i < 0 {
			return 3
		}
		pos := off + i
		if pos > 0 && strings.IndexByte(" _-.(", name[pos-1]) >= 0 {
			return 2
		}
		off = pos + 1
	}
}
