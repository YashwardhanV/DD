package phone

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/yashwardhanv/dd/internal/index"
)

// maxMalformedKept is how many unparseable lines are remembered for diagnostics.
const maxMalformedKept = 20

// Parser turns the lines printed by the phone's `find`/`stat` into index
// entries.
//
// `find -type f` only prints FILES, but the index also needs the folders they
// sit in (a path is rebuilt from parent IDs). So whenever a file appears in a
// folder we haven't seen yet, the folder - and any missing parents above it -
// is created on the spot.
//
// A consequence worth knowing: folders that contain no files are not indexed.
type Parser struct {
	b      *index.Builder
	prefix string // the scanned root with a trailing "/", e.g. "/sdcard/"

	dirs   map[string]uint64 // folder path relative to root -> ID ("" = root = 0)
	nextID uint64

	Files, Dirs    int64
	Malformed      int64
	FirstMalformed []string
}

// NewParser creates a parser adding to b. root is the remote folder that was
// listed (such as "/sdcard/"); it is removed from the front of every path.
func NewParser(b *index.Builder, root string) *Parser {
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return &Parser{b: b, prefix: root, dirs: map[string]uint64{"": 0}}
}

// Entries is the number of files plus synthesized folders so far.
func (p *Parser) Entries() int64 { return p.Files + p.Dirs }

func (p *Parser) malformed(line string) {
	p.Malformed++
	if len(p.FirstMalformed) < maxMalformedKept {
		p.FirstMalformed = append(p.FirstMalformed, line)
	}
}

// relPath removes the scanned root from a remote path.
func (p *Parser) relPath(remote string) string {
	rel, ok := strings.CutPrefix(remote, p.prefix)
	if !ok {
		// find sometimes prints the root itself without the trailing slash, or a
		// path under a differently-spelled root; keep what we can.
		rel = remote
	}
	return cleanRel(rel)
}

// cleanRel tidies a path relative to the root: "a//b", "./a" and "a/../b"
// would otherwise create folders with empty or "."/".." names. A path that
// ends in "/" keeps that slash, so addFile can see it isn't a file.
func cleanRel(rel string) string {
	if rel == "" {
		return ""
	}
	trailing := strings.HasSuffix(rel, "/")
	c := strings.TrimLeft(path.Clean("/"+rel), "/")
	if trailing && c != "" {
		c += "/"
	}
	return c
}

// ParseStatLine handles one line of `stat -c '%s|%Y|%n'`: size|mtime|path.
// The path is last and may itself contain "|". Returns false (and counts the
// line as malformed) if the line isn't in that shape.
func (p *Parser) ParseStatLine(line string) bool {
	line = strings.TrimRight(line, "\r") // older adb versions end lines with \r\n
	if line == "" {
		return false // blank lines are not an error, just empty
	}
	parts := strings.SplitN(line, "|", 3)
	if len(parts) != 3 {
		p.malformed(line)
		return false
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	rel := p.relPath(parts[2])
	if err1 != nil || err2 != nil || rel == "" || size < 0 {
		p.malformed(line)
		return false
	}
	p.addFile(rel, size, mtime, 0)
	return true
}

// ParseNameLine handles one line of the fallback `find` (names only): just a
// path. Size and date are unknown.
func (p *Parser) ParseNameLine(line string) bool {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return false
	}
	rel := p.relPath(line)
	if rel == "" {
		p.malformed(line)
		return false
	}
	p.addFile(rel, -1, 0, index.FlagNoMeta)
	return true
}

// addFile adds a file and (if needed) its folders.
func (p *Parser) addFile(rel string, size, mtime int64, flags uint8) {
	dir, name := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		dir, name = rel[:i], rel[i+1:]
	}
	if name == "" { // path ended with "/": not a file
		p.malformed(rel)
		return
	}
	parent := p.ensureDir(dir)
	p.nextID++
	p.b.Add(index.Entry{ID: p.nextID, ParentID: parent, Name: name, Size: size, ModTime: mtime, Flags: flags})
	p.Files++
}

// ensureDir returns the ID of folder rel, creating it and its ancestors.
func (p *Parser) ensureDir(rel string) uint64 {
	if id, ok := p.dirs[rel]; ok {
		return id
	}
	parentRel, name := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		parentRel, name = rel[:i], rel[i+1:]
	}
	parent := p.ensureDir(parentRel)
	p.nextID++
	id := p.nextID
	p.dirs[rel] = id
	p.b.Add(index.Entry{ID: id, ParentID: parent, Name: name, Size: 0, Flags: index.FlagDir})
	p.Dirs++
	return id
}

// summary is used in error messages.
func (p *Parser) summary() string {
	return fmt.Sprintf("%d files, %d malformed lines", p.Files, p.Malformed)
}
