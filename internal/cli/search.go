package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/query"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

const searchLong = `Searches every saved index in memory - drives that are not plugged in are included
and marked offline. Words must all appear in the name (not case sensitive).

  "two words"                 keep words together
  !word                       leave out names containing word
  ext:jpg,png                 only these extensions
  type:file | type:dir        only files / only folders
  size:>100mb  size:<1kb  size:10mb..1gb
  modified:>2025-01-01  modified:<30d      (<30d = within the last 30 days)
  drive:E  drive:phone  drive:device  drive:"SANDISK 32GB"
  path:whatsapp               look in the whole path, not just the name
  re:^IMG_\d{8}               a regular expression on the name

For scripts: --json prints an array of results, --paths-only one path per line,
and --copy-to <folder> copies every match (not just the ones shown).`

// newSearchCmd implements `drivedex search <terms…>`: search every saved index
// in memory. This is the payoff of indexing - it never touches a drive.
func newSearchCmd() *cobra.Command {
	var f searchFlags

	cmd := &cobra.Command{
		Use:   "search <terms…>",
		Short: "Search all saved indexes instantly",
		Long:  searchLong,
		Example: `  drivedex search resume pdf
  drivedex search ext:jpg size:>2mb modified:<7d --json
  drivedex search drive:phone IMG --paths-only
  drivedex search invoice !draft --copy-to D:\Invoices`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.filesOnly && f.dirsOnly {
				return userErr("--files and --dirs can't be used together", "pick one (or neither)", nil)
			}
			if f.json && f.pathsOnly {
				return userErr("--json and --paths-only can't be used together", "pick one", nil)
			}
			if f.copyTo != "" && (f.json || f.pathsOnly) {
				return userErr("--copy-to can't be combined with --json or --paths-only", "run the search again with just --copy-to", nil)
			}
			f.limitSet = cmd.Flags().Changed("limit")
			return runSearch(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), strings.Join(args, " "), f)
		},
	}
	cmd.Flags().IntVar(&f.limit, "limit", 50, "show at most N results (0 = all; the default comes from default_limit in config.toml)")
	cmd.Flags().StringVar(&f.source, "source", "", "only search this index (an id from 'drivedex list')")
	cmd.Flags().StringVar(&f.drive, "drive", "", "only search the drive currently at this letter, e.g. E:")
	cmd.Flags().BoolVar(&f.filesOnly, "files", false, "only show files")
	cmd.Flags().BoolVar(&f.dirsOnly, "dirs", false, "only show folders")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the results as a JSON array (for scripts)")
	cmd.Flags().BoolVar(&f.pathsOnly, "paths-only", false, "print only the paths, one per line (for piping)")
	cmd.Flags().StringVar(&f.copyTo, "copy-to", "", "copy every matching file into this folder")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "with --copy-to: don't ask before copying more than 100 files")
	return cmd
}

type searchFlags struct {
	limit     int
	limitSet  bool
	source    string
	drive     string
	filesOnly bool
	dirsOnly  bool
	json      bool
	pathsOnly bool
	copyTo    string
	yes       bool
}

// jsonResult is one element of `search --json`.
type jsonResult struct {
	Path     string  `json:"path"`     // a path you can use: real path if the drive is connected, where it was last seen if not, phone:/... for phones
	Name     string  `json:"name"`     // just the file or folder name
	Size     *int64  `json:"size"`     // bytes; null if unknown
	Modified *string `json:"modified"` // RFC 3339 (UTC); null if unknown
	SourceID string  `json:"source_id"`
	Label    string  `json:"label"`
	Online   bool    `json:"online"` // is the drive/phone connected right now?
}

func runSearch(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, text string, f searchFlags) error {
	machine := f.json || f.pathsOnly
	// Human messages must never end up inside machine-readable output.
	info := stdout
	if machine {
		info = stderr
	}

	q, err := query.Parse(text, time.Now())
	if err != nil {
		return userErr(err.Error(), "see 'drivedex search --help' for the search syntax", err)
	}
	if f.filesOnly {
		q.Type = query.FilesOnly
	}
	if f.dirsOnly {
		q.Type = query.DirsOnly
	}
	if q.IsEmpty() {
		return userErr("the search is empty", `type at least one word, e.g. drivedex search resume`, nil)
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	loadStart := time.Now()
	loaded, err := store.LoadAll()
	if err != nil {
		return userErr("couldn't read the saved indexes", "check the folder exists and is readable: "+store.Dir, err)
	}
	loadTime := time.Since(loadStart)
	printWarnings(info, loaded.Warnings)

	if len(loaded.Indexes) == 0 {
		fmt.Fprintln(info, `Nothing indexed yet. Try: drivedex index E:\`)
		if f.json {
			fmt.Fprintln(stdout, "[]")
		}
		return nil
	}

	env := currentEnv(hasPhone(loaded.Indexes))
	m := q.Matcher(env)
	m.HideHidden = !activeConfig.ShowHidden
	var matcher index.Matcher = m
	sourceID := f.source
	if f.drive != "" {
		if sourceID, err = sourceIDForLetter(f.drive); err != nil {
			return err
		}
	}
	if sourceID != "" {
		matcher = &index.SourceFilter{Inner: m, SourceID: sourceID}
	}

	limit := f.limit
	if !f.limitSet {
		limit = activeConfig.DefaultLimit
	}
	if f.copyTo != "" {
		limit = 0 // copy EVERY match, not just the ones that would be shown
	}

	searchStart := time.Now()
	res := index.Search(loaded.Indexes, matcher, index.SearchOptions{Limit: limit})
	searchTime := time.Since(searchStart)

	out := bufio.NewWriterSize(stdout, 64*1024)
	defer out.Flush()

	switch {
	case f.copyTo != "":
		out.Flush()
		err = copyMatches(ctx, stdin, out, loaded.Indexes, res, env, f)
	case f.json:
		err = writeJSON(out, loaded.Indexes, res, env)
	case f.pathsOnly:
		forEachHit(loaded.Indexes, res, env, func(ix *index.Index, h index.Hit, rel string) {
			fmt.Fprintln(out, index.CopyPath(&ix.Header, rel, env))
		})
	default:
		writeTable(out, loaded.Indexes, res, env, searchTime)
	}
	if err != nil {
		return err
	}

	if !machine && f.copyTo == "" {
		printStaleTips(out, loaded.Indexes, matcher, env, time.Now())
		if flagDebug {
			fmt.Fprintln(out, ui.Dim("loaded indexes in "+ui.FormatDuration(loadTime)))
		}
	}
	if q.UsesMetadata() && m.SkippedNoMeta() > 0 {
		out.Flush()
		fmt.Fprintln(info, ui.Dim(noMetaTip(loaded.Indexes, env)))
	}
	return nil
}

// forEachHit calls fn for every hit with the entry's path relative to its source root.
func forEachHit(indexes []*index.Index, res index.SearchResult, env index.Env, fn func(ix *index.Index, h index.Hit, rel string)) {
	caches := make([]map[uint64]string, len(indexes)) // folder-path caches, one per index
	for _, h := range res.Hits {
		ix := indexes[h.Index]
		if caches[h.Index] == nil {
			caches[h.Index] = map[uint64]string{}
		}
		fn(ix, h, ix.RelPathCached(h.Slot, caches[h.Index]))
	}
}

// writeTable prints the human-readable results and the footer.
func writeTable(out io.Writer, indexes []*index.Index, res index.SearchResult, env index.Env, took time.Duration) {
	forEachHit(indexes, res, env, func(ix *index.Index, h index.Hit, rel string) {
		path := index.DisplayPath(&ix.Header, rel, env)
		lazyMetadata(ix, h.Slot, rel, env)

		size := "<dir>"
		if ix.Flags[h.Slot]&index.FlagDir == 0 {
			size = ui.HumanSize(ix.Sizes[h.Slot])
		}
		fmt.Fprintf(out, "%s  %s\n", path, ui.Dim(fmt.Sprintf("%9s  %s", size, ui.FormatTime(ix.MTimes[h.Slot]))))
	})
	fmt.Fprintf(out, "\n%s of %s matches · %s · searched %s entries in %d %s\n",
		ui.Thousands(int64(len(res.Hits))), ui.Thousands(int64(res.Total)), ui.FormatMillis(took),
		ui.Thousands(int64(res.Searched)), res.Indexes, plural(res.Indexes, "index", "indexes"))
}

// writeJSON prints the results as a JSON array.
func writeJSON(out io.Writer, indexes []*index.Index, res index.SearchResult, env index.Env) error {
	results := make([]jsonResult, 0, len(res.Hits)) // never null, even with no results
	forEachHit(indexes, res, env, func(ix *index.Index, h index.Hit, rel string) {
		lazyMetadata(ix, h.Slot, rel, env)
		hd := &ix.Header
		r := jsonResult{
			Path: index.CopyPath(hd, rel, env), Name: ix.Names[h.Slot],
			SourceID: hd.SourceID, Label: hd.Label, Online: env.Online(hd),
		}
		isDir := ix.Flags[h.Slot]&index.FlagDir != 0
		if isDir {
			zero := int64(0)
			r.Size = &zero
		} else if ix.Flags[h.Slot]&index.FlagNoMeta == 0 && ix.Sizes[h.Slot] >= 0 {
			size := ix.Sizes[h.Slot]
			r.Size = &size
		}
		if ix.Flags[h.Slot]&index.FlagNoMeta == 0 && ix.MTimes[h.Slot] != 0 {
			mod := time.Unix(ix.MTimes[h.Slot], 0).UTC().Format(time.RFC3339)
			r.Modified = &mod
		}
		results = append(results, r)
	})
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep < > & readable in names
	return enc.Encode(results)
}

// noMetaTip explains why size:/modified: filters skipped some entries.
func noMetaTip(indexes []*index.Index, env index.Env) string {
	drive := "E:"
	for _, ix := range indexes {
		if root, mounted := env.Mounts[ix.Header.SourceID]; mounted && ix.Header.Scanner == "mft" {
			drive = strings.TrimRight(root, `\/`)
			break
		}
	}
	return fmt.Sprintf("tip: some NTFS turbo-mode entries have no size/date; run 'drivedex index %s --with-metadata' to include them.", drive)
}

// plural picks the singular or plural form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// sourceIDForLetter finds the index ID of the drive currently at a letter.
func sourceIDForLetter(arg string) (string, error) {
	letter, ok := volume.NormalizeLetter(arg)
	if !ok {
		return "", userErr(fmt.Sprintf("%q isn't a drive letter", arg), "use something like --drive E:", nil)
	}
	vols, _ := listVolumes()
	for _, v := range vols {
		if v.Ready && strings.EqualFold(v.Letter(), letter) {
			return volume.SourceID(v), nil
		}
	}
	return "", userErr(fmt.Sprintf("drive %s isn't connected", letter),
		"for an unplugged drive use --source <id> (see 'drivedex list')", nil)
}

// printStaleTips reminds the user to refresh the index of any connected drive
// that was last indexed more than stale_after ago.
func printStaleTips(out io.Writer, indexes []*index.Index, m index.Matcher, env index.Env, now time.Time) {
	staleAfter := activeStaleAfter()
	for _, ix := range indexes {
		h := &ix.Header
		root, mounted := env.Mounts[h.SourceID]
		if h.SourceKind != index.KindVolume || !mounted || !m.Applies(h) {
			continue
		}
		if age := now.Sub(h.UpdatedAt); age > staleAfter {
			letter := strings.TrimRight(root, `\/`)
			fmt.Fprintln(out, ui.Dim(fmt.Sprintf("tip: %s was indexed %s — run 'drivedex index %s' to refresh",
				root, ui.Ago(h.UpdatedAt, now), letter)))
		}
	}
}

// hasPhone reports whether any loaded index belongs to a phone.
func hasPhone(indexes []*index.Index) bool {
	for _, ix := range indexes {
		if ix.Header.SourceKind == index.KindPhone {
			return true
		}
	}
	return false
}

// lazyMetadata fills in the size and date of a displayed result whose index
// doesn't have them (NTFS turbo mode stores names only). Only the few results
// being shown are looked up, and only when the drive is connected - so this
// costs next to nothing. Phones are skipped: their paths aren't files on this PC.
func lazyMetadata(ix *index.Index, slot int, rel string, env index.Env) {
	if ix.Flags[slot]&index.FlagNoMeta == 0 {
		return
	}
	if path, ok := index.LocalPath(&ix.Header, rel, env); ok {
		ix.FillMeta(slot, path)
	}
}
