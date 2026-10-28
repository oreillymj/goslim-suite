package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"goslim/internal/upnp"
)

// flags holds the parsed command line.
type flags struct {
	list        bool
	mac         string
	name        string
	host        string
	port        int
	mdns        bool
	mdnsName    string
	timeout     time.Duration
	bind        net.IP
	httpPort    int
	clen        int64
	idle        time.Duration
	test        bool
	volOffset   int
	volMax      int
	info        bool
	verbose     bool
	probeWindow time.Duration

	showVersion bool
	logFile     string
	logTime     bool
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet("goupnpaudioclient", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: goupnpaudioclient [options]

  goupnpaudioclient                          list the UPnP renderers and exit
  goupnpaudioclient -m aa:bb:cc:dd:ee:ff     stream a GoSlim proxy player to that renderer
  goupnpaudioclient -test                    play GoSlim's audio on this PC (no renderer needed)

Options:
`)
		fs.PrintDefaults()
	}
	fs.BoolVar(&f.list, "l", false, "list renderers and exit (also the default when -m is not given)")
	fs.StringVar(&f.mac, "m", "", "MAC address of the renderer to stream to")
	fs.StringVar(&f.name, "n", "", "player name to present to LMS via GoSlim (default: renderer's friendly name up to the first ' - ')")
	fs.StringVar(&f.host, "host", "127.0.0.1", "host running GoSlim in proxy mode")
	fs.IntVar(&f.port, "p", 3434, "GoSlim proxy port (host defaults to localhost)")
	fs.BoolVar(&f.mdns, "mdns", true, "if GoSlim does not answer on -host:-p, look for it with mDNS (GoSlim announces the port it got)")
	fs.StringVar(&f.mdnsName, "mdnsname", "", "with mDNS: only use a GoSlim whose announced name contains this text (default: the first found,\npreferring the one at -host)")
	timeout := fs.Int("t", 4, "discovery timeout in seconds")
	bind := fs.String("bind", "", "local IPv4 address to run discovery from (useful with several adapters)")
	fs.IntVar(&f.httpPort, "httpport", 0, "local TCP port for the audio stream served to the renderer (0 = any)")
	fs.Int64Var(&f.clen, "clen", 0, "advertise this Content-Length for the WAV stream (0 = chunked; some renderers need a large fixed value)")
	idle := fs.Int("idle", 5, "seconds without audio from GoSlim before the stream is ended")
	fs.BoolVar(&f.test, "test", false, "play the audio from GoSlim on this PC instead of a renderer (no discovery; -m and -n are optional)")
	fs.IntVar(&f.volOffset, "voloffset", 0, "add this (positive or negative) to the volume LMS asks for, in renderer percent")
	fs.IntVar(&f.volMax, "volmax", 100, "never set the renderer louder than this percent")
	fs.BoolVar(&f.info, "info", false, "with -m: print the renderer's zone group / stereo pair and per-channel volume details, then exit")
	fs.DurationVar(&f.probeWindow, "reconnectreplay", upnp.ProbeWindow, "a renderer that opens a stream, closes it and opens it again (GStreamer-based ones do) gets the stream from\nthe start again if it comes back within this time of its first request, however much it had read. 0 turns this off")
	fs.BoolVar(&f.verbose, "v", false, "verbose output")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
	fs.StringVar(&f.logFile, "logfile", "", "also append log output to this file")
	fs.BoolVar(&f.logTime, "logtime", true, "prefix log lines with a timestamp (use -logtime=false under systemd, which adds its own)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	f.timeout = time.Duration(*timeout) * time.Second
	f.idle = time.Duration(*idle) * time.Second
	if *bind != "" {
		if f.bind = net.ParseIP(*bind).To4(); f.bind == nil {
			return nil, fmt.Errorf("-bind: %q is not an IPv4 address", *bind)
		}
	}
	return f, nil
}
