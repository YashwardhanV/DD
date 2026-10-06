// Package phonetest provides a fake adb for tests, so phone features can be
// tested on any machine without a phone (or adb) attached.
package phonetest

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/yashwardhanv/dd/internal/phone"
)

// Fake is a phone.Runner whose answers come from a Handler function.
type Fake struct {
	// Handler is called for every adb invocation with the adb arguments (for
	// example ["devices", "-l"] or ["-s", "SERIAL", "shell", "find ..."]).
	// It returns what adb would print on stdout and, optionally, an error
	// such as &phone.ExitError{Code: 1}.
	Handler func(ctx context.Context, args []string) (stdout string, err error)

	mu    sync.Mutex
	calls [][]string
}

// Calls returns every invocation so far (copies of the argument lists).
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func (f *Fake) record(args []string) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
}

// Output implements phone.Runner.
func (f *Fake) Output(ctx context.Context, args ...string) ([]byte, error) {
	f.record(args)
	out, err := f.Handler(ctx, args)
	return []byte(out), err
}

// Stream implements phone.Runner.
func (f *Fake) Stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	f.record(args)
	out, err := f.Handler(ctx, args)
	return io.NopCloser(strings.NewReader(out)), func() error { return err }, nil
}

// New returns an *phone.ADB backed by the handler.
func New(handler func(ctx context.Context, args []string) (string, error)) (*phone.ADB, *Fake) {
	f := &Fake{Handler: handler}
	return phone.NewADBWithRunner(f), f
}

// DevicesOutput builds the text `adb devices -l` prints for the given lines.
func DevicesOutput(lines ...string) string {
	return "List of devices attached\n" + strings.Join(lines, "\n") + "\n\n"
}

// Pixel is a typical line for an authorized phone.
const Pixel = "R58M1234ABC            device usb:1-1 product:panther model:Pixel_7 device:panther transport_id:1"
