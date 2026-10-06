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
)

// newSearchCmd implements `drivedex search <terms…>`: search every saved index
// in memory. This is the payoff of indexing - it never touches a drive.
func newSearchCmd() *cobra.Command {
	var limit int
	var source string
	var filesOnly, dirsOnly bool

	cmd := &cobra.Command{
		Use:   "search <terms…>",
		Short: "Search all saved indexes instantly",
		Long: `Finds files and folders whose name contains every search term (not case
sensitive). Results from drives that are not plugged in are included and marked
offline. Put "double quotes" around a phrase to keep its words together.`,
		Example: `  drivedex search resume
  drivedex search foto jpg --limit 10
  drivedex search "final report" --files`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filesOnly && dirsOnly {
				return userErr("--files and --dirs can't be used together", "pick one (or neither)", nil)
			}
			return runSearch(cmd.OutOrStdout(), strings.Join(args, " "), searchFlags{
				limit: limit, source: source, filesOnly: filesOnly, dirsOnly: dirsOnly,
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "show at most N results (0 = all)")
	cmd.Flags().StringVar(&source, "source", "", "only search this index (an id from 'drivedex list')")
	cmd.Flags().BoolVar(&filesOnly, "files", false, "only show files")
	cmd.Flags().BoolVar(&dirsOnly, "dirs", false, "only show folders")
	return cmd
}

type searchFlags struct {
	limit     int
	source    string
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

	m := index.NewTermMatcher(query)
	m.SourceID, m.OnlyFiles, m.OnlyDirs = f.source, f.filesOnly, f.dirsOnly
	if len(m.Terms) == 0 {
		return userErr("the search is empty", `type at least one word, e.g. drivedex search resume`, nil)
	}

	searchStart := time.Now()
	res := index.Search(loaded.Indexes, m, index.SearchOptions{Limit: f.limit})
	searchTime := time.Since(searchStart)

	out := bufio.NewWriterSize(stdout, 64*1024)
	defer out.Flush()

	env := buildEnv()
	caches := make([]map[uint64]string, len(loaded.Indexes)) // folder-path caches, one per index
	for _, h := range res.Hits {
		ix := loaded.Indexes[h.Index]
		if caches[h.Index] == nil {
			caches[h.Index] = map[uint64]string{}
		}
		rel := ix.RelPathCached(h.Slot, caches[h.Index])
		path := index.DisplayPath(&ix.Header, rel, env)

		size := "<dir>"
		if ix.Flags[h.Slot]&index.FlagDir == 0 {
			size = ui.HumanSize(ix.Sizes[h.Slot])
		}
		fmt.Fprintf(out, "%s  %s\n", path, ui.Dim(fmt.Sprintf("%9s  %s", size, ui.FormatTime(ix.MTimes[h.Slot]))))
	}

	fmt.Fprintf(out, "\n%s of %s matches · %s · searched %s entries in %d %s\n",
		ui.Thousands(int64(len(res.Hits))), ui.Thousands(int64(res.Total)), ui.FormatMillis(searchTime),
		ui.Thousands(int64(res.Searched)), res.Indexes, plural(res.Indexes, "index", "indexes"))
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
