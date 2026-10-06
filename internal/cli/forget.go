package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// newForgetCmd implements `drivedex forget <id|letter|label>`.
func newForgetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget <id|letter|label>",
		Short: "Delete a saved index (the drive itself is never touched)",
		Long: `Deletes DriveDex's saved index for a drive, folder or phone. Only DriveDex's
own index file is removed - nothing on the drive changes. Name the index by its
id (see 'drivedex list'), its drive letter, or its label.`,
		Example: `  drivedex forget vol-1A2B3C4D
  drivedex forget E:
  drivedex forget "SANDISK 32GB" --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForget(cmd.OutOrStdout(), cmd.InOrStdin(), args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

func runForget(out io.Writer, in io.Reader, target string, yes bool) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	infos, err := store.List()
	if err != nil {
		return userErr("couldn't read the saved indexes", "check the folder exists and is readable: "+store.Dir, err)
	}
	vols, _ := listVolumes()

	matches := matchIndexes(infos, vols, target)
	switch len(matches) {
	case 0:
		return userErr(fmt.Sprintf("no saved index matches %q", target), "run 'drivedex list' to see the ids, letters and labels", nil)
	case 1:
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, infoID(m))
		}
		return userErr(fmt.Sprintf("%q matches %d indexes (%s)", target, len(matches), strings.Join(ids, ", ")),
			"use the exact id from 'drivedex list'", nil)
	}
	m := matches[0]
	id := infoID(m)

	name := id
	if m.Err == nil {
		name = fmt.Sprintf("%q (%s, %s entries)", describeIndex(m.Header), id, ui.Thousands(int64(m.Header.EntryCount)))
	}
	if !yes {
		fmt.Fprintf(out, "Delete the saved index %s?\nThis removes only DriveDex's own file; the drive is not touched. [y/N] ", name)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Cancelled. Nothing was deleted.")
			return nil
		}
	}
	// Delete by the file we matched, so unreadable files can be removed too.
	if err := os.Remove(m.Path); err != nil {
		return userErr("couldn't delete the index file", "close any program using it and try again: "+m.Path, err)
	}
	fmt.Fprintf(out, "Forgot %s. The drive itself was not touched.\n", name)
	return nil
}

// matchIndexes finds saved indexes the user's text refers to. It tries, in
// order, and stops at the first kind of match that finds anything:
//
//  1. the exact index id (or file name)
//  2. a drive letter: the drive plugged in at that letter now, else drives last seen there
//  3. the label
func matchIndexes(infos []index.Info, vols []volume.Volume, target string) []index.Info {
	target = strings.TrimSpace(target)
	collect := func(pred func(index.Info) bool) []index.Info {
		var out []index.Info
		for _, in := range infos {
			if pred(in) {
				out = append(out, in)
			}
		}
		return out
	}

	if m := collect(func(in index.Info) bool {
		return strings.EqualFold(infoID(in), target) ||
			strings.EqualFold(filepath.Base(in.Path), target)
	}); len(m) > 0 {
		return m
	}

	if letter, ok := volume.NormalizeLetter(target); ok {
		for _, v := range vols {
			if v.Ready && strings.EqualFold(v.Letter(), letter) {
				id := volume.SourceID(v)
				if m := collect(func(in index.Info) bool { return in.Err == nil && in.Header.SourceID == id }); len(m) > 0 {
					return m
				}
			}
		}
		if m := collect(func(in index.Info) bool {
			return in.Err == nil && in.Header.SourceKind == index.KindVolume && strings.EqualFold(letterOf(in.Header.LastMount), letter)
		}); len(m) > 0 {
			return m
		}
	}

	return collect(func(in index.Info) bool {
		return in.Err == nil && strings.EqualFold(strings.TrimSpace(in.Header.Label), target)
	})
}
