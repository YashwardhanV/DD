package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yashwardhanv/dd/internal/portable"
	"github.com/yashwardhanv/dd/internal/volume"
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
	// Nor may they depend on what is plugged into the developer's PC: no
	// portable device, and every drive's bus unknown, unless a test says otherwise.
	listDevices = func() ([]portable.Device, error) { return nil, nil }
	busTypeFor = func(string) (volume.BusInfo, error) { return volume.BusInfo{}, nil }
	systemDrive = func() string { return "C:" }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
