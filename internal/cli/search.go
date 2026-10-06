package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// newSearchCmd implements `drivedex search <terms…>`: search every saved index
// in memory. This is the payoff of indexing - it never touches a drive.
func newSearchCmd() *cobra.Command {
	var limit int
	var source, drive string
	var filesOnly, dirsOnly bool

	cmd := &cobra.Command{
		Use:   "search <terms…>",
		Short: "Search all saved indexes instantly",
		Long: `Finds files and folders whose name contains every search term (not case
sensitive). Results from drives that are not plugged in are included and marked
offline. Put "double quotes" around a phrase to keep its words together.`,
		Example: `  drivedex search resume
  drivedex search foto jpg --limit 10
  drivedex search invoice --drive E:
  drivedex search "final report" --files`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filesOnly && dirsOnly {
				return userErr("--files and --dirs can't be used together", "pick one (or neither)", nil)
			}
			return runSearch(cmd.OutOrStdout(), strings.Join(args, " "), searchFlags{
				limit: limit, source: source, drive: drive, filesOnly: filesOnly, dirsOnly: dirsOnly,
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "show at most N results (0 = all)")
	cmd.Flags().StringVar(&source, "source", "", "only search this index (an id from 'drivedex list')")
	cmd.Flags().StringVar(&drive, "drive", "", "only search the drive currently at this letter, e.g. E:")
	cmd.Flags().BoolVar(&filesOnly, "files", false, "only show files")
	cmd.Flags().BoolVar(&dirsOnly, "dirs", false, "only show folders")
	return cmd
}

type searchFlags struct {
	limit     int
	source    string
	drive     string
	filesOnly bool
	dirsOnly  bool
}

func runSearch(stdout io.Writer, query string, f searchFlags) error {
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
	printWarnings(stdout, loaded.Warnings)

	if len(loaded.Indexes) == 0 {
		fmt.Fprintln(stdout, `Nothing indexed yet. Try: drivedex index E:\`)
		return nil
	}

	env := currentEnv(hasPhone(loaded.Indexes))
	m := index.NewTermMatcher(query)
	m.SourceID, m.OnlyFiles, m.OnlyDirs = f.source, f.filesOnly, f.dirsOnly
	if f.drive != "" {
		id, err := sourceIDForLetter(f.drive)
		if err != nil {
			return err
		}
		m.SourceID = id
	}
	if len(m.Terms) == 0 {
		return userErr("the search is empty", `type at least one word, e.g. drivedex search resume`, nil)
	}

	searchStart := time.Now()
	res := index.Search(loaded.Indexes, m, index.SearchOptions{Limit: f.limit})
	searchTime := time.Since(searchStart)

	out := bufio.NewWriterSize(stdout, 64*1024)
	defer out.Flush()

	caches := make([]map[uint64]string, len(loaded.Indexes)) // folder-path caches, one per index
	for _, h := range res.Hits {
		ix := loaded.Indexes[h.Index]
		if caches[h.Index] == nil {
			caches[h.Index] = map[uint64]string{}
		}
		rel := ix.RelPathCached(h.Slot, caches[h.Index])
		path := index.DisplayPath(&ix.Header, rel, env)
		lazyMetadata(ix, h.Slot, rel, env)

		size := "<dir>"
		if ix.Flags[h.Slot]&index.FlagDir == 0 {
			size = ui.HumanSize(ix.Sizes[h.Slot])
		}
		fmt.Fprintf(out, "%s  %s\n", path, ui.Dim(fmt.Sprintf("%9s  %s", size, ui.FormatTime(ix.MTimes[h.Slot]))))
	}

	fmt.Fprintf(out, "\n%s of %s matches · %s · searched %s entries in %d %s\n",
		ui.Thousands(int64(len(res.Hits))), ui.Thousands(int64(res.Total)), ui.FormatMillis(searchTime),
		ui.Thousands(int64(res.Searched)), res.Indexes, plural(res.Indexes, "index", "indexes"))
	printStaleTips(out, loaded.Indexes, m, env, time.Now())
	if flagDebug {
		fmt.Fprintln(out, ui.Dim("loaded indexes in "+ui.FormatDuration(loadTime)))
	}
	return nil
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
// that was last indexed more than staleAfter ago.
func printStaleTips(out io.Writer, indexes []*index.Index, m *index.TermMatcher, env index.Env, now time.Time) {
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
	h := &ix.Header
	switch h.SourceKind {
	case index.KindVolume:
		if root, mounted := env.Mounts[h.SourceID]; mounted {
			ix.FillMeta(slot, index.JoinRoot(root, rel))
		}
	case index.KindFolder:
		ix.FillMeta(slot, index.JoinRoot(h.RootPath, rel))
	}
}
