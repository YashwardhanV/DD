// Command drivedex is the DriveDex command-line tool.
//
// This file is intentionally tiny: it only wires up Ctrl+C handling and hands
// control to the cli package, where every command lives.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/yashwardhanv/dd/internal/cli"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	// A context that is cancelled when the user presses Ctrl+C. Every long
	// operation receives it, so they can all stop cleanly instead of being
	// killed in the middle of writing a file.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	code := cli.Execute(ctx, version)

	stop()
	os.Exit(code)
}
