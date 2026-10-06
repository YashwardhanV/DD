package cli

import (
	"fmt"
	"io"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
)

// openStore opens the folder where indexes are saved.
func openStore() (*index.Store, error) {
	s, err := index.OpenDefault()
	if err != nil {
		return nil, userErr("can't find the folder for saved indexes", "set the DRIVEDEX_INDEX_DIR environment variable to a writable folder", err)
	}
	return s, nil
}

// printWarnings shows non-fatal problems (e.g. a corrupt index that was skipped).
func printWarnings(w io.Writer, warnings []error) {
	for _, e := range warnings {
		fmt.Fprintln(w, ui.Yellow("warning:")+" skipped an index: "+e.Error())
		fmt.Fprintln(w, ui.Dim("  Fix: re-index that drive with 'drivedex index <drive>' (or remove the file with 'drivedex forget')."))
	}
}
