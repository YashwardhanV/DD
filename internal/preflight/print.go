package preflight

import (
	"fmt"
	"io"
	"strings"

	"github.com/yashwardhanv/dd/internal/ui"
)

// shownPaths is how many paths each issue lists in normal output.
const shownPaths = 5

// Print writes the issues, most serious first. Every path is listed only with
// verbose (dry runs and --verbose); otherwise at most five per issue.
func (r Report) Print(w io.Writer, verbose bool) {
	for _, is := range r.Issues {
		var mark string
		switch is.Severity {
		case Block:
			mark = ui.Red("✗")
		case Warn:
			mark = ui.Yellow("⚠")
		default:
			mark = ui.Dim("ℹ")
		}
		line := is.Message
		if is.Fixed != "" {
			line += " " + ui.Dim("("+is.Fixed+")")
		}
		fmt.Fprintf(w, "%s %s\n", mark, line)
		limit := shownPaths
		if verbose {
			limit = len(is.Paths)
		}
		for i, p := range is.Paths {
			if i >= limit {
				break
			}
			fmt.Fprintf(w, "    %s\n", p)
		}
		if more := is.Count - min(limit, len(is.Paths)); more > 0 && len(is.Paths) > 0 {
			fmt.Fprintf(w, "    %s\n", ui.Dim(fmt.Sprintf("… and %s more", ui.Thousands(int64(more)))))
		}
		if is.Hint != "" {
			fmt.Fprintf(w, "    %s %s\n", ui.Dim("Fix:"), strings.ReplaceAll(is.Hint, "\n", "\n         "))
		}
	}
}
