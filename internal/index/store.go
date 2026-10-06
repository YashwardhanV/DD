package index

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EnvIndexDir overrides where indexes are stored. It exists mainly so tests
// (and curious owners) can use a throw-away folder.
const EnvIndexDir = "DRIVEDEX_INDEX_DIR"

// Store is the folder of saved .ddx files, one per source.
type Store struct{ Dir string }

// DefaultDir is %LOCALAPPDATA%\DriveDex\indexes on Windows (the user cache
// directory), unless DRIVEDEX_INDEX_DIR is set.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvIndexDir); d != "" {
		return d, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding the cache folder: %w", err)
	}
	return filepath.Join(cache, "DriveDex", "indexes"), nil
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// OpenDefault returns the store in the default location.
func OpenDefault() (*Store, error) {
	d, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return NewStore(d), nil
}

// fileName turns a SourceID into a safe file name: <SourceID>.ddx, with any
// character Windows doesn't allow in file names (such as the ":" in a
// wireless adb serial "192.168.1.5:5555") replaced by "_".
func fileName(sourceID string) string {
	var b strings.Builder
	for _, r := range sourceID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String() + ".ddx"
}

// Path is where the index for sourceID lives.
func (s *Store) Path(sourceID string) string { return filepath.Join(s.Dir, fileName(sourceID)) }

// LockPath is the lock file used while an index is being rebuilt
// ("<id>.lock" next to "<id>.ddx"). See the agent package for how it is used.
func (s *Store) LockPath(sourceID string) string {
	return strings.TrimSuffix(s.Path(sourceID), ".ddx") + ".lock"
}

// Save writes the index atomically and returns the file size.
//
// It writes to "<id>.ddx.tmp" and then renames over "<id>.ddx", so a crash or
// power cut halfway through can never leave a half-written index behind: a
// reader sees either the complete old file or the complete new one.
func (s *Store) Save(ix *Index) (int64, error) {
	if ix.Header.SourceID == "" {
		return 0, errors.New("saving index: header has no SourceID")
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return 0, fmt.Errorf("creating index folder %s: %w", s.Dir, err)
	}
	final := s.Path(ix.Header.SourceID)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", tmp, err)
	}
	if err := Write(f, ix); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("replacing %s: %w", final, err)
	}
	st, err := os.Stat(final)
	if err != nil {
		return 0, fmt.Errorf("checking %s: %w", final, err)
	}
	return st.Size(), nil
}

// Load reads the index for one source.
func (s *Store) Load(sourceID string) (*Index, error) { return ReadFile(s.Path(sourceID)) }

// Exists reports whether an index file for sourceID is present.
func (s *Store) Exists(sourceID string) bool {
	_, err := os.Stat(s.Path(sourceID))
	return err == nil
}

// Delete removes the saved index for a source. It never touches the drive the
// index was made from.
func (s *Store) Delete(sourceID string) error {
	if err := os.Remove(s.Path(sourceID)); err != nil {
		return fmt.Errorf("deleting index %s: %w", sourceID, err)
	}
	return nil
}

// Loaded is the result of LoadAll.
type Loaded struct {
	Indexes  []*Index
	Warnings []error // files that were skipped (corrupt, unreadable)
}

// ddxFiles lists the .ddx files in the store, sorted by name.
func (s *Store) ddxFiles() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // nothing indexed yet
	}
	if err != nil {
		return nil, fmt.Errorf("reading index folder %s: %w", s.Dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".ddx") {
			files = append(files, filepath.Join(s.Dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// LoadAll loads every index in the store, one goroutine per file. A corrupt
// file is skipped and reported in Warnings rather than failing everything.
func (s *Store) LoadAll() (Loaded, error) {
	files, err := s.ddxFiles()
	if err != nil {
		return Loaded{}, err
	}
	results := make([]*Index, len(files))
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	for i, path := range files {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			results[i], errs[i] = ReadFile(path)
		}(i, path)
	}
	wg.Wait()

	var out Loaded
	for i := range files {
		if errs[i] != nil {
			out.Warnings = append(out.Warnings, errs[i])
			continue
		}
		out.Indexes = append(out.Indexes, results[i])
	}
	return out, nil
}

// Info describes one saved index without loading its entries.
type Info struct {
	Header   Header
	Path     string
	FileSize int64
	ModTime  time.Time
	Err      error // set when the header could not be read
}

// List returns information about every saved index, quickly (headers only).
func (s *Store) List() ([]Info, error) {
	files, err := s.ddxFiles()
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(files))
	for _, path := range files {
		info := Info{Path: path}
		if st, err := os.Stat(path); err == nil {
			info.FileSize, info.ModTime = st.Size(), st.ModTime()
		}
		info.Header, info.Err = ReadHeaderFile(path)
		infos = append(infos, info)
	}
	return infos, nil
}
