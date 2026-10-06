package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// walk visits every command below c that a user can run.
func walk(c *cobra.Command, fn func(*cobra.Command)) {
	for _, sub := range c.Commands() {
		if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
			continue // cobra's own helpers
		}
		fn(sub)
		walk(sub, fn)
	}
}

// Every command explains itself: a one-line Short, a plain-English Long and
// real Examples (BUILD.md stage 11).
func TestEveryCommandHasShortLongAndExamples(t *testing.T) {
	root := NewRootCmd("test")
	count := 0
	walk(root, func(c *cobra.Command) {
		count++
		name := c.CommandPath()
		if strings.TrimSpace(c.Short) == "" {
			t.Errorf("%s: no Short", name)
		}
		if strings.HasSuffix(strings.TrimSpace(c.Short), ".") {
			t.Errorf("%s: Short should not end with a full stop: %q", name, c.Short)
		}
		if len(strings.Split(strings.TrimSpace(c.Long), "\n")) < 2 {
			t.Errorf("%s: Long should be a few lines of plain English, got %q", name, c.Long)
		}
		if n := len(nonEmptyLines(c.Example)); n < 2 {
			t.Errorf("%s: wants 2-3 examples, has %d", name, n)
		}
	})
	if count < 15 {
		t.Errorf("only %d commands found - is the walk broken?", count)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestCompletionForPowerShellIsAvailable(t *testing.T) {
	out, err := run(t, "completion", "powershell")
	if err != nil || !strings.Contains(out, "Register-ArgumentCompleter") {
		t.Fatalf("completion powershell: err=%v\n%.200s", err, out)
	}
}

func TestHelpMentionsNewCommands(t *testing.T) {
	out, _ := run(t, "--help")
	for _, want := range []string{"doctor", "config", "search", "copy", "agent", "shell", "completion"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help should list %q:\n%s", want, out)
		}
	}
}
