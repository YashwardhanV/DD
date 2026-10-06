package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps every test away from the real settings file: DriveDex creates
// config.toml on first use, and the tests must not write into the developer's
// own settings folder.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "drivedex-cli-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("DRIVEDEX_CONFIG", filepath.Join(dir, "config.toml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
