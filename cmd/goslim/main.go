// GoSlim is a Squeezebox (Slimproto) player for Lyrion / Logitech Media Server.
// It plays on the local sound device, or, with -proxyport, accepts
// GoUPnPAudioClient connections and presents each one to LMS as its own player.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"goslim/internal/app"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if f.showVersion {
		fmt.Println(app.VersionLine("goslim", version))
		return
	}
	os.Exit(app.Run(app.Options{Name: "goslim", Version: version, LogFile: f.logFile, LogTime: f.logTime},
		func(ctx context.Context) error { return run(ctx, f) }))
}
