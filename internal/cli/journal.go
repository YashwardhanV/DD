package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// newJournalCmd implements `drivedex journal status|enable <drive>`.
func newJournalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Check or switch on an NTFS drive's change journal (Administrator)",
		Long: `NTFS can keep a small diary of every change made to the drive (its "change
journal"). DriveDex reads that diary to refresh an index in milliseconds. These
commands show whether a drive has one and let you switch it on. They need an
Administrator terminal.`,
		Example: `  drivedex journal status C:
  drivedex journal enable E:`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newJournalStatusCmd(), newJournalEnableCmd())
	return cmd
}

func newJournalStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <drive>",
		Short: "Show whether a drive's change journal is on",
		Long: `Reports whether the NTFS drive has an active change journal, and if so its
size and position. Needs an Administrator terminal because Windows only lets
administrators open a drive for raw reading.`,
		Example: `  drivedex journal status C:
  drivedex journal status E:`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalStatus(cmd.OutOrStdout(), args[0])
		},
	}
}

func newJournalEnableCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "enable <drive>",
		Short: "Switch on a drive's change journal (adds a 32 MB change log)",
		Long: `Creates the NTFS change journal on a drive that doesn't have one, so DriveDex
can use turbo mode and fast refreshes there. It adds a small (32 MB) change log
to the drive's file system.`,
		Example: `  drivedex journal enable E:
  drivedex journal enable E: --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalEnable(cmd.OutOrStdout(), cmd.InOrStdin(), args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

// openJournalTarget checks the drive is NTFS and we are Administrator, then
// opens it. It returns the drive root such as `E:\`.
func openJournalTarget(arg string) (*ntfs.Session, string, error) {
	letter, ok := volume.NormalizeLetter(arg)
	if !ok {
		return nil, "", userErr(fmt.Sprintf("%q isn't a drive letter", arg), "use something like E: or E:\\", nil)
	}
	root := letter + `\`
	v, err := volumeForPath(root)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		return nil, "", userErr("the change journal can only be checked on Windows", "run DriveDex on your Windows PC", err)
	case err != nil:
		return nil, "", userErr(fmt.Sprintf("can't find drive %s", letter), "run 'drivedex drives' to see what is connected", err)
	case !v.Ready:
		return nil, "", userErr(fmt.Sprintf("drive %s isn't ready", letter), "is a disk inserted?", nil)
	case !strings.EqualFold(v.FileSystem, "NTFS"):
		return nil, "", userErr(fmt.Sprintf("drive %s is %s, not NTFS", letter, v.FileSystem), "only NTFS drives have a change journal", nil)
	}
	if !isElevated() {
		return nil, "", userErr("this needs an Administrator terminal",
			"right-click PowerShell and choose \"Run as administrator\", then run the command again", nil)
	}
	sess, err := openNTFS(root)
	if err != nil {
		if errors.Is(err, ntfs.ErrNeedAdmin) {
			return nil, "", userErr("this needs an Administrator terminal", "right-click PowerShell and choose \"Run as administrator\"", err)
		}
		return nil, "", userErr(fmt.Sprintf("couldn't open drive %s", letter), "check the drive is connected; run with --debug for details", err)
	}
	return sess, root, nil
}

func runJournalStatus(out io.Writer, arg string) error {
	sess, root, err := openJournalTarget(arg)
	if err != nil {
		return err
	}
	defer sess.Close()

	jd, err := ntfs.QueryJournal(sess.Dev)
	if errors.Is(err, ntfs.ErrJournalNotActive) || errors.Is(err, ntfs.ErrJournalDeleteInProgress) {
		fmt.Fprintf(out, "Change journal on %s is NOT active.\n", root)
		fmt.Fprintf(out, "Switch it on (Administrator) with: drivedex journal enable %s\n", letterOrRoot(root))
		return nil
	}
	if err != nil {
		return userErr("couldn't read the change journal", "run with --debug for details", err)
	}
	fmt.Fprintf(out, "Change journal on %s is active.\n", root)
	fmt.Fprintf(out, "  ID:            0x%016X\n", jd.ID)
	fmt.Fprintf(out, "  Size:          %s (grows in %s steps)\n", ui.HumanSizeU(jd.MaximumSize), ui.HumanSizeU(jd.AllocationDelta))
	fmt.Fprintf(out, "  First USN:     %s\n", ui.Thousands(jd.FirstUsn))
	fmt.Fprintf(out, "  Next USN:      %s\n", ui.Thousands(jd.NextUsn))
	fmt.Fprintf(out, "  Lowest valid:  %s\n", ui.Thousands(jd.LowestValidUsn))
	return nil
}

func runJournalEnable(out io.Writer, in io.Reader, arg string, yes bool) error {
	sess, root, err := openJournalTarget(arg)
	if err != nil {
		return err
	}
	defer sess.Close()

	if _, err := ntfs.QueryJournal(sess.Dev); err == nil {
		fmt.Fprintf(out, "The change journal on %s is already active. Nothing to do.\n", root)
		return nil
	}

	if !yes {
		fmt.Fprint(out, "This adds a small change log (32 MB) to the drive's file system. Continue? y/N ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Cancelled. Nothing was changed.")
			return nil
		}
	}
	if err := ntfs.CreateJournal(sess.Dev, 32<<20, 4<<20); err != nil {
		return userErr("couldn't create the change journal", "run with --debug for details; the drive may be read-only", err)
	}
	fmt.Fprintf(out, "Change journal switched on for %s. Now run: drivedex index %s\n", root, letterOrRoot(root))
	return nil
}
