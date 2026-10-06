package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newListCmd implements `drivedex list`.
func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every saved index (including drives that are unplugged)",
		Long: `Shows all indexes DriveDex has saved: which drive or folder each one is for,
how many entries it holds, when it was last updated and whether the drive is
connected right now.`,
		Example: `  drivedex list
  drivedex forget vol-1A2B3C4D   (remove an index you no longer need)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.OutOrStdout())
		},
	}
}

func runList(out io.Writer) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	infos, err := store.List()
	if err != nil {
		return userErr("couldn't read the saved indexes", "check the folder exists and is readable: "+store.Dir, err)
	}
	if len(infos) == 0 {
		fmt.Fprintln(out, `Nothing indexed yet. Try: drivedex index E:\`)
		return nil
	}

	wantPhones := false
	for _, in := range infos {
		wantPhones = wantPhones || (in.Err == nil && in.Header.SourceKind == index.KindPhone)
	}
	env := currentEnv(wantPhones)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tKIND\tFS\tENTRIES\tUPDATED\tSTATUS\tLAST MOUNT\tFILE SIZE")
	for _, in := range infos {
		if in.Err != nil {
			id := strings.TrimSuffix(filepath.Base(in.Path), filepath.Ext(in.Path))
			fmt.Fprintf(tw, "%s\t(unreadable)\t-\t-\t-\t-\t-\t-\t%s\n", id, ui.HumanSize(in.FileSize))
			continue
		}
		h := in.Header
		status := "offline"
		if env.Online(&h) {
			status = "online"
		}
		lastMount := h.LastMount
		if lastMount == "" {
			lastMount = "-"
		}
		fs := h.FileSystem
		if fs == "" {
			fs = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			h.SourceID, describeIndex(h), h.SourceKind, fs, ui.Thousands(int64(h.EntryCount)),
			ui.FormatTime(h.UpdatedAt.Unix()), status, lastMount, ui.HumanSize(in.FileSize))
	}
	return tw.Flush()
}

// infoID returns the ID of a saved index (its SourceID, or the file name for
// an unreadable file).
func infoID(in index.Info) string {
	if in.Err == nil {
		return in.Header.SourceID
	}
	return strings.TrimSuffix(filepath.Base(in.Path), filepath.Ext(in.Path))
}
