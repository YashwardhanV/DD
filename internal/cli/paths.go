package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveRoot turns what the user typed into a clean absolute path.
//
//	E:      -> E:\      ("E:" alone means "current folder on E:" to Windows,
//	                     but a user typing it means the whole drive)
//	.\docs  -> C:\...\docs
func resolveRoot(arg string) (string, error) {
	p := strings.TrimSpace(arg)
	// A bare drive letter like "E:" becomes the drive root "E:\".
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", arg, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", userErr(fmt.Sprintf("can't open %s", arg),
			"check the path exists (and that the drive is plugged in)", err)
	}
	if !st.IsDir() {
		return "", userErr(fmt.Sprintf("%s is a file, not a folder", arg), "give a folder or a drive such as E:\\", nil)
	}
	return abs, nil
}
