// Command drivedex-agent is the background program that indexes external
// drives, phones and other portable devices automatically when they are
// plugged in.
//
// It is built as a Windows "GUI" program (-ldflags -H=windowsgui) so it has no
// console window at all. Start and stop it with `drivedex agent start|stop`.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/yashwardhanv/dd/internal/agent"
)

// version is overridden at build time: go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := agent.Main(ctx, version)
	stop()
	os.Exit(code)
}
