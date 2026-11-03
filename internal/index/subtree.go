package index

import (
	"strings"
)

// Children maps every folder ID to the slots of its direct children. Build it
// once (O(n)) when many folders' contents are needed, instead of scanning the
// whole index per folder.
type Children map[uint64][]int32

// BuildChildren builds the folder -> children map, skipping deleted entries.
func (ix *Index) BuildChildren() Children {
	c := make(Children, len(ix.IDs)/8+1)
	for slot := range ix.IDs {
		if ix.Flags[slot]&FlagDeleted != 0 {
			continue
		}
		p := ix.Parents[slot]
		c[p] = append(c[p], int32(slot))
	}
	return c
}

// WalkFolder calls fn for every entry inside the folder with ID folderID (0 =
// the root), at any depth, parents before their children. rel is the entry's
// path relative to that folder with `\` separators. fn returning false stops
// the walk. Cycles in corrupt data can't loop forever: each folder is entered
// at most once.
func (ix *Index) WalkFolder(children Children, folderID uint64, fn func(slot int, rel string) bool) {
	type item struct {
		id  uint64
		rel string
	}
	seen := map[uint64]bool{folderID: true}
	stack := []item{{folderID, ""}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, s := range children[cur.id] {
			slot := int(s)
			rel := ix.Names[slot]
			if cur.rel != "" {
				rel = cur.rel + `\` + rel
			}
			if !fn(slot, rel) {
				return
			}
			if ix.Flags[slot]&FlagDir != 0 && !seen[ix.IDs[slot]] {
				seen[ix.IDs[slot]] = true
				stack = append(stack, item{ix.IDs[slot], rel})
			}
		}
	}
}

// PathUpdate is one file to put into an index by its path.
type PathUpdate struct {
	Rel     string // relative to the index root; `\` or `/` separators
	Size    int64
	ModTime int64
}

// UpsertPaths adds files by path, or updates the size and time of files that
// are already there, creating any missing folders on the way. New entries get
// fresh IDs above the highest one in use. It returns how many files were
// added or changed. Names are matched case-insensitively.
//
// Only use it on indexes whose IDs DriveDex assigned itself (phone, walk and
// parallel scans): an NTFS turbo-mode index uses the file system's own IDs.
func (ix *Index) UpsertPaths(updates []PathUpdate) int {
	type key struct {
		parent uint64
		name   string
	}
	byName := make(map[key]int, len(ix.IDs))
	var maxID uint64
	for slot, id := range ix.IDs {
		if id > maxID {
			maxID = id
		}
		if ix.Flags[slot]&FlagDeleted == 0 {
			byName[key{ix.Parents[slot], ix.Lower[slot]}] = slot
		}
	}
	changed := 0
	for _, u := range updates {
		parts := strings.FieldsFunc(u.Rel, func(r rune) bool { return r == '\\' || r == '/' })
		if len(parts) == 0 {
			continue
		}
		parent := uint64(0)
		ok := true
		for _, dir := range parts[:len(parts)-1] {
			k := key{parent, strings.ToLower(dir)}
			if slot, found := byName[k]; found {
				if ix.Flags[slot]&FlagDir == 0 {
					ok = false // a file is in the way: leave it alone
					break
				}
				parent = ix.IDs[slot]
				continue
			}
			maxID++
			ix.Add(Entry{ID: maxID, ParentID: parent, Name: dir, Flags: FlagDir})
			byName[k] = len(ix.IDs) - 1
			parent = maxID
		}
		if !ok {
			continue
		}
		name := parts[len(parts)-1]
		k := key{parent, strings.ToLower(name)}
		if slot, found := byName[k]; found {
			if ix.Flags[slot]&FlagDir != 0 {
				continue
			}
			if ix.Sizes[slot] == u.Size && ix.MTimes[slot] == u.ModTime && ix.Flags[slot]&FlagNoMeta == 0 {
				continue
			}
			e := ix.Entry(slot)
			e.Size, e.ModTime, e.Flags = u.Size, u.ModTime, e.Flags&^FlagNoMeta
			ix.Upsert(e)
			changed++
			continue
		}
		maxID++
		ix.Add(Entry{ID: maxID, ParentID: parent, Name: name, Size: u.Size, ModTime: u.ModTime})
		byName[k] = len(ix.IDs) - 1
		changed++
	}
	return changed
}
