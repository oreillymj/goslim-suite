// Package app is the process harness shared by the commands: a version
// banner, logging setup, and a context that is cancelled when the OS asks the
// process to stop. That covers Ctrl+C, SIGTERM from systemd, and the stop
// request NSSM sends a console program on Windows (Ctrl+C), so the programs run
// as services without any service-specific code.
package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

// Options configure Run.
type Options struct {
	Name    string // program name
	Version string // set by the build: -ldflags "-X main.version=..."
	LogFile string // also append log output to this file
	LogTime bool   // prefix log lines with a timestamp (turn off under journald)
}

// ShutdownTimeout is how long a program gets to stop after being asked to.
const ShutdownTimeout = 10 * time.Second

// VersionLine describes this build.
func VersionLine(name, version string) string {
	return fmt.Sprintf("%s %s (%s, %s/%s)", name, version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Run sets up logging, prints the version, and calls run with a context that is
// cancelled on SIGINT or SIGTERM. run should return once its context is done;
// if it takes longer than ShutdownTimeout Run gives up. A second signal during
// shutdown kills the process the normal way. It returns the process exit code.
func Run(o Options, run func(ctx context.Context) error) int {
	flags := 0
	if o.LogTime {
		flags = log.LstdFlags
	}
	log.SetFlags(flags)
	var w io.Writer = os.Stderr
	if o.LogFile != "" {
		f, err := os.OpenFile(o.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open log file: %v\n", err)
			return 1
		}
		defer f.Close()
		w = io.MultiWriter(os.Stderr, f)
	}
	log.SetOutput(w)
	log.Print(VersionLine(o.Name, o.Version))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- run(ctx) }()

	select {
	case err := <-errc:
		return finish(err)
	case <-ctx.Done():
		stop() // restore default signal handling: a second Ctrl+C now kills us
		log.Printf("stopping")
		select {
		case err := <-errc:
			return finish(err)
		case <-time.After(ShutdownTimeout):
			log.Printf("shutdown timed out after %s", ShutdownTimeout)
			return 1
		}
	}
}

func finish(err error) int {
	if err != nil {
		log.Printf("error: %v", err)
		return 1
	}
	return 0
}
