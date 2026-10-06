package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// sharedReader wraps in so that several questions in a row read from ONE
// buffer. (A fresh bufio.Reader per question would swallow all piped input on
// the first read and answer "no" to every later question.) bufio.NewReader
// returns an existing large-enough *bufio.Reader unchanged, so sub-commands
// that wrap it again keep sharing it.
func sharedReader(in io.Reader) *bufio.Reader { return bufio.NewReader(in) }

// askYesNo prints the question followed by " [y/N] " and reads one line.
// Anything but "y" or "yes" (including no input at all) means no, so a script
// that forgets --yes can never accidentally say yes.
func askYesNo(out io.Writer, in io.Reader, question string) bool {
	fmt.Fprint(out, question+" [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes"
}
