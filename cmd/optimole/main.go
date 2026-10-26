// Command optimole uploads local files into an Optimole media library.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/Codeinwp/optimole-cli/internal/cli"
)

// Set by the release build:
// -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-10-26T12:00:00Z"
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first Ctrl-C the run winds down and prints its summary; a
	// second one gets the default behaviour and ends the process.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := cli.Run(ctx, os.Args[1:], cli.DefaultEnv(buildInfo()))
	stop()
	os.Exit(code)
}

// buildInfo returns the ldflags values, falling back to what the Go toolchain
// recorded (a "go install ...@v1.2.3" build has no ldflags).
func buildInfo() cli.BuildInfo {
	b := cli.BuildInfo{Version: version, Commit: commit, Date: date}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	if b.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		b.Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && b.Commit == "":
			b.Commit = s.Value
			if len(b.Commit) > 12 {
				b.Commit = b.Commit[:12]
			}
		case s.Key == "vcs.time" && b.Date == "":
			b.Date = s.Value
		}
	}
	return b
}
