package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// newDrivesCmd implements `drivedex drives`.
func newDrivesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drives",
		Short: "List connected drives and whether each one is indexed",
		Long: `Shows every drive currently connected - pen drives, external disks, internal
disks - with its file system, size and free space, and whether DriveDex has an
index for it.`,
		Example: `  drivedex drives
  drivedex index E:   (index a drive you see here)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDrives(cmd.OutOrStdout())
		},
	}
}

func runDrives(out io.Writer) error {
	vols, err := listVolumes()
	if errors.Is(err, errors.ErrUnsupported) {
		return userErr("listing drives only works on Windows", "run DriveDex on your Windows PC", err)
	}
	if err != nil {
		return userErr("couldn't list the drives", "try again; if it keeps failing run with --debug", err)
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	infos, err := store.List()
	if err != nil {
		return userErr("couldn't read the saved indexes", "check the folder exists and is readable: "+store.Dir, err)
	}
	byID := map[string]index.Info{}
	for _, in := range infos {
		if in.Err == nil {
			byID[in.Header.SourceID] = in
		}
	}

	now := time.Now()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DRIVE\tLABEL\tFS\tTYPE\tSIZE\tFREE\tINDEXED?")
	for _, v := range vols {
		if !v.Ready {
			fmt.Fprintf(tw, "%s\t(not ready)\t-\t%s\t-\t-\t-\n", v.Letter(), v.TypeName())
			continue
		}
		indexed := "no"
		if in, ok := byID[volume.SourceID(v)]; ok {
			indexed = indexedSummary(in.Header, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			v.Letter(), v.Label, v.FileSystem, v.TypeName(), ui.HumanSizeU(v.TotalBytes), ui.HumanSizeU(v.FreeBytes), indexed)
	}
	return tw.Flush()
}
