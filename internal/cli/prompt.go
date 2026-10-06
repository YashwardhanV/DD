package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// askYesNo prints the question followed by " [y/N] " and reads one line.
// Anything but "y" or "yes" (including no input at all) means no, so a script
// that forgets --yes can never accidentally say yes.
func askYesNo(out io.Writer, in io.Reader, question string) bool {
	fmt.Fprint(out, question+" [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes"
}
