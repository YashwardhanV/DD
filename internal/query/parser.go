// Package query parses DriveDex's search language and turns it into an
// index.Matcher.
//
// Plain words behave as before (every word must appear in the name, in any
// case). On top of that:
//
//	"two words"            a phrase
//	!word                  exclude names containing word
//	ext:jpg,png            extension is one of these
//	type:file | type:dir   only files / only folders
//	size:>100mb  size:<1kb  size:10mb..1gb     (b, kb, mb, gb, tb - 1024-based)
//	modified:>2025-01-01   modified:<30d       (a date, or a number of days: <30d = within the last 30 days)
//	drive:E  drive:phone  drive:device  drive:"SANDISK 32GB" which drive, phone or device
//	path:word              match anywhere in the full path, not only the name
//	re:<regex>             a Go regular expression (not case sensitive) on the name
//
// Anything else with a colon (like  foo:bar ) is just text to look for.
package query

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// TypeFilter restricts results to files or folders.
type TypeFilter int

const (
	AnyType TypeFilter = iota
	FilesOnly
	DirsOnly
)

// Range is an inclusive range of int64 values; a missing end is open.
type Range struct {
	Min, Max       int64
	HasMin, HasMax bool
}

// Contains reports whether v lies in the range.
func (r Range) Contains(v int64) bool {
	return (!r.HasMin || v >= r.Min) && (!r.HasMax || v <= r.Max)
}

// DriveFilter selects one source. Exactly one field is set.
type DriveFilter struct {
	Letter string // "E:" - the drive currently (or last) at that letter
	Phone  bool   // any phone: over USB debugging, or as a portable device
	Device bool   // any portable device (phone in "File transfer" mode, iPhone, camera...)
	Name   string // a label, or an index id such as vol-1A2B3C4D (not case sensitive)
}

// Query is a parsed search.
type Query struct {
	Terms     []string // words that must appear in the name (lowercase)
	Exclude   []string // words that must NOT appear in the name (lowercase)
	Exts      []string // allowed extensions (lowercase, no dot); empty = any
	Type      TypeFilter
	Size      *Range // bytes
	Modified  *Range // unix seconds
	Drives    []DriveFilter
	PathTerms []string // lowercase, `\` separators
	Regexes   []*regexp.Regexp
}

// SyntaxError is a problem in the search text, worded to say how to fix it.
type SyntaxError struct {
	Token string
	Msg   string
}

func (e *SyntaxError) Error() string { return e.Msg }

func syntaxErr(token, format string, args ...any) error {
	return &SyntaxError{Token: token, Msg: fmt.Sprintf(format, args...)}
}

// IsEmpty reports whether the query has no criteria at all (so it should show
// nothing rather than every file).
func (q *Query) IsEmpty() bool {
	return len(q.Terms) == 0 && len(q.Exclude) == 0 && len(q.Exts) == 0 && q.Type == AnyType &&
		q.Size == nil && q.Modified == nil && len(q.Drives) == 0 && len(q.PathTerms) == 0 && len(q.Regexes) == 0
}

// UsesMetadata reports whether the query filters on size or modification date.
func (q *Query) UsesMetadata() bool { return q.Size != nil || q.Modified != nil }

// knownKeys are the filters that take a value.
var knownKeys = map[string]bool{"ext": true, "type": true, "size": true, "modified": true, "drive": true, "path": true, "re": true}

// Parse reads a search. now is the time that relative dates ("<7d") count back from.
func Parse(text string, now time.Time) (*Query, error) {
	q := &Query{}
	for _, raw := range splitTokens(text) {
		if err := q.addToken(raw, now); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// splitTokens cuts the text at spaces that are not inside double quotes. The
// quote characters stay in the tokens (their meaning is decided later).
func splitTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// unquote removes the quote characters from a token's text.
func unquote(s string) string { return strings.ReplaceAll(s, `"`, "") }

func (q *Query) addToken(raw string, now time.Time) error {
	negate := false
	rest := raw
	if strings.HasPrefix(rest, "!") && len(rest) > 1 {
		negate, rest = true, rest[1:]
	}

	// key:value ?  (the key must come before any quote and be a known filter)
	if i := strings.IndexByte(rest, ':'); i > 0 && !strings.Contains(rest[:i], `"`) {
		if key := strings.ToLower(rest[:i]); knownKeys[key] {
			if negate {
				return syntaxErr(raw, "'!' can only be put in front of plain words, not of %s:", key)
			}
			return q.addFilter(key, unquote(rest[i+1:]), raw, now)
		}
	}

	word := strings.ToLower(strings.TrimSpace(unquote(rest)))
	if word == "" {
		return nil // an empty "" pair or a lone '!'
	}
	if negate {
		q.Exclude = append(q.Exclude, word)
	} else {
		q.Terms = append(q.Terms, word)
	}
	return nil
}

func (q *Query) addFilter(key, value, raw string, now time.Time) error {
	if strings.TrimSpace(value) == "" {
		return syntaxErr(raw, "%s: needs a value, for example %s", key, filterExample(key))
	}
	switch key {
	case "ext":
		for _, e := range strings.Split(value, ",") {
			if e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), ".")); e != "" {
				q.Exts = append(q.Exts, e)
			}
		}
		if len(q.Exts) == 0 {
			return syntaxErr(raw, "ext: needs a value, for example ext:jpg,png")
		}
	case "type":
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "file", "files", "f":
			q.Type = FilesOnly
		case "dir", "dirs", "folder", "folders", "d":
			q.Type = DirsOnly
		default:
			return syntaxErr(raw, "type: must be file or dir, not %q", value)
		}
	case "size":
		r, err := parseSize(value)
		if err != nil {
			return syntaxErr(raw, "%v", err)
		}
		q.Size = r
	case "modified":
		r, err := parseModified(value, now)
		if err != nil {
			return syntaxErr(raw, "%v", err)
		}
		q.Modified = r
	case "drive":
		q.Drives = append(q.Drives, parseDrive(value))
	case "path":
		p := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "/", `\`))
		q.PathTerms = append(q.PathTerms, p)
	case "re":
		re, err := regexp.Compile("(?i)" + value)
		if err != nil {
			return syntaxErr(raw, "re: isn't a valid regular expression (%v)", firstWords(err.Error()))
		}
		q.Regexes = append(q.Regexes, re)
	}
	return nil
}

func firstWords(s string) string {
	s = strings.TrimPrefix(s, "error parsing regexp: ")
	return s
}

func filterExample(key string) string {
	switch key {
	case "ext":
		return "ext:jpg,png"
	case "type":
		return "type:dir"
	case "size":
		return "size:>100mb"
	case "modified":
		return "modified:<7d"
	case "drive":
		return "drive:E or drive:phone"
	case "path":
		return "path:whatsapp"
	case "re":
		return `re:^IMG_\d{8}`
	}
	return key + ":value"
}

// ---- size -----------------------------------------------------------------------------

var sizeRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(b|kb|mb|gb|tb|k|m|g|t)?$`)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40,
}

func parseSizeValue(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("can't understand the size %q in size: - try size:>100mb, size:<1kb or size:10mb..1gb", s)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	return saturate(n * sizeUnits[strings.ToLower(m[2])]), nil
}

// saturate converts a non-negative float to int64, capping it at MaxInt64.
// (Converting a too-big float straight to int64 gives a garbage - often
// negative - number, which would turn size:>99999tb into "every file".)
func saturate(f float64) int64 {
	if f >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(f)
}

// parseSize understands >100mb  <1kb  >=1gb  <=5mb  =0  10mb..1gb  ..1gb  10mb..
func parseSize(v string) (*Range, error) {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, ".."); i >= 0 {
		r := &Range{}
		if lo := strings.TrimSpace(v[:i]); lo != "" {
			n, err := parseSizeValue(lo)
			if err != nil {
				return nil, err
			}
			r.Min, r.HasMin = n, true
		}
		if hi := strings.TrimSpace(v[i+2:]); hi != "" {
			n, err := parseSizeValue(hi)
			if err != nil {
				return nil, err
			}
			r.Max, r.HasMax = n, true
		}
		if !r.HasMin && !r.HasMax {
			return nil, fmt.Errorf("size: range needs at least one end, e.g. size:10mb..1gb")
		}
		if r.HasMin && r.HasMax && r.Min > r.Max {
			return nil, fmt.Errorf("the range in size:%s is backwards (the first size is bigger than the second)", v)
		}
		return r, nil
	}
	op, num := splitOperator(v)
	n, err := parseSizeValue(num)
	if err != nil {
		return nil, err
	}
	switch op {
	case ">":
		if n == math.MaxInt64 { // nothing is bigger; n+1 would wrap around to "everything"
			return &Range{Min: n, Max: n - 1, HasMin: true, HasMax: true}, nil
		}
		return &Range{Min: n + 1, HasMin: true}, nil
	case ">=":
		return &Range{Min: n, HasMin: true}, nil
	case "<":
		return &Range{Max: n - 1, HasMax: true}, nil
	case "<=":
		return &Range{Max: n, HasMax: true}, nil
	default: // "=" or none
		return &Range{Min: n, Max: n, HasMin: true, HasMax: true}, nil
	}
}

// splitOperator separates a leading >, <, >=, <= or = from the rest.
func splitOperator(v string) (op, rest string) {
	for _, o := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(v, o) {
			return o, strings.TrimSpace(v[len(o):])
		}
	}
	return "", v
}

// ---- dates ------------------------------------------------------------------------------

var (
	dateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	relativeRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(h|d|w)$`)
)

func parseDay(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q isn't a date - write it as YYYY-MM-DD, like 2025-01-31", s)
	}
	return t, nil
}

// parseModified understands:
//
//	>2025-01-01   after that day          <2025-01-01   before that day
//	>=2025-01-01  from that day on        <=2025-01-01  up to the end of that day
//	=2025-01-01   during that day         2025-01-01..2025-06-30  between (both days included)
//	<30d          within the last 30 days  >30d          more than 30 days ago   (also h = hours, w = weeks)
func parseModified(v string, now time.Time) (*Range, error) {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, ".."); i >= 0 {
		lo, err := parseDay(strings.TrimSpace(v[:i]))
		if err != nil {
			return nil, err
		}
		hi, err := parseDay(strings.TrimSpace(v[i+2:]))
		if err != nil {
			return nil, err
		}
		if hi.Before(lo) {
			return nil, fmt.Errorf("the range in modified:%s is backwards (the first date is later than the second)", v)
		}
		return &Range{Min: lo.Unix(), HasMin: true, Max: hi.AddDate(0, 0, 1).Unix() - 1, HasMax: true}, nil
	}
	op, val := splitOperator(v)

	if m := relativeRe.FindStringSubmatch(val); m != nil {
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]float64{"h": 3600, "d": 24 * 3600, "w": 7 * 24 * 3600}[strings.ToLower(m[2])]
		// Count back in seconds, not time.Duration: a Duration tops out at
		// about 292 years, so modified:<300000d would silently mean "<292 years".
		// Half of MaxInt64 seconds is far beyond any date and can't overflow.
		cutoff := now.Unix() - min(saturate(n*unit), math.MaxInt64/2)
		switch op {
		case "<", "<=": // younger than N days: modified AFTER the cutoff
			return &Range{Min: cutoff, HasMin: true}, nil
		case ">", ">=": // older than N days: modified BEFORE the cutoff
			return &Range{Max: cutoff - 1, HasMax: true}, nil
		}
		return nil, fmt.Errorf("modified:%s needs < or > in front, e.g. modified:<30d (within the last 30 days)", v)
	}

	if !dateRe.MatchString(val) {
		return nil, fmt.Errorf("can't understand modified:%s - use a date like modified:>2025-01-01 or a number of days like modified:<30d", v)
	}
	day, err := parseDay(val)
	if err != nil {
		return nil, err
	}
	start, end := day.Unix(), day.AddDate(0, 0, 1).Unix()
	switch op {
	case ">":
		return &Range{Min: end, HasMin: true}, nil
	case ">=":
		return &Range{Min: start, HasMin: true}, nil
	case "<":
		return &Range{Max: start - 1, HasMax: true}, nil
	case "<=":
		return &Range{Max: end - 1, HasMax: true}, nil
	default:
		return &Range{Min: start, Max: end - 1, HasMin: true, HasMax: true}, nil
	}
}

// ---- drive ---------------------------------------------------------------------------------

func parseDrive(v string) DriveFilter {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "phone") || strings.EqualFold(v, "phones") {
		return DriveFilter{Phone: true}
	}
	if strings.EqualFold(v, "device") || strings.EqualFold(v, "devices") {
		return DriveFilter{Device: true}
	}
	if len(v) <= 3 {
		s := v
		if len(s) == 1 {
			s += ":"
		}
		s = strings.TrimRight(s, `\/`)
		if len(s) == 2 && s[1] == ':' && unicode.IsLetter(rune(s[0])) {
			return DriveFilter{Letter: strings.ToUpper(s)}
		}
	}
	return DriveFilter{Name: v}
}
