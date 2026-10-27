// GoUPnPAudioClient discovers UPnP/DLNA audio renderers and, when given -m,
// pulls decoded WAV audio from a GoSlim proxy port and plays it on that renderer.
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
		fmt.Println(app.VersionLine("goupnpaudioclient", version))
		return
	}
	os.Exit(app.Run(app.Options{Name: "goupnpaudioclient", Version: version, LogFile: f.logFile, LogTime: f.logTime},
		func(ctx context.Context) error { return run(ctx, f) }))
}
