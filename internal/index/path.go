package index

// maxPathDepth stops runaway loops if corrupt data ever contains a parent
// cycle (A is inside B which is inside A).
const maxPathDepth = 4096

// RelPath rebuilds the path of a slot, relative to the source root, using `\`
// as the separator (e.g. `Photos\2024\img.jpg`).
//
// It walks up the parent IDs until it reaches 0 (the root). If a parent is
// missing from the index (an "orphan") the path starts with `?\` instead.
func (ix *Index) RelPath(slot int) string {
	return ix.RelPathCached(slot, nil)
}

// RelPathCached is RelPath with a cache of folder paths keyed by folder ID.
// Pass the same map for many calls (for example while printing 50 search
// results from the same folder) and each folder's path is built only once.
func (ix *Index) RelPathCached(slot int, cache map[uint64]string) string {
	return joinPath(ix.folderPath(ix.Parents[slot], cache, 0), ix.Names[slot])
}

// folderPath returns the path of the folder with the given ID ("" for the
// root). Missing folders yield "?" so the final path starts with `?\`.
func (ix *Index) folderPath(id uint64, cache map[uint64]string, depth int) string {
	if id == 0 {
		return ""
	}
	if cache != nil {
		if p, ok := cache[id]; ok {
			return p
		}
	}
	var p string
	slot, ok := ix.byID[id]
	if !ok || depth > maxPathDepth {
		p = "?"
	} else {
		p = joinPath(ix.folderPath(ix.Parents[slot], cache, depth+1), ix.Names[slot])
	}
	if cache != nil {
		cache[id] = p
	}
	return p
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + `\` + name
}
