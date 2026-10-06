package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCommand checks that `drivedex version` prints the version string
// and the platform it was built for.
func TestVersionCommand(t *testing.T) {
	root := NewRootCmd("v9.9.9")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "drivedex v9.9.9 (") {
		t.Errorf("unexpected version output: %q", got)
	}
}
