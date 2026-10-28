package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"goslim/internal/config"
	"goslim/internal/upnp"
)

// flags holds the parsed command line.
type flags struct {
	// Slimproto side
	server     string
	name       string
	mac        string
	rate       int
	maxRate    int
	webPort    int
	webUser    string
	webPass    string
	replayGain bool
	ffmpeg     string
	nativeAAC  bool
	nativeALAC bool
	nativeOgg  bool

	// UPnP side
	configFile    string
	list          bool
	asJSON        bool
	jsonFile      config.OptString
	info          bool
	infoMAC       string
	timeout       time.Duration
	bind          net.IP
	httpStartPort int
	clen          int64
	idle          time.Duration
	volOffset     int
	volMax        int
	buffer        float64
	decodeAudio   bool
	resampleAudio bool
	autoResume    bool
	icy           bool
	exitOnNetFail bool
	verbose       bool
	probeWindow   time.Duration

	// process
	showVersion bool
	logFile     string
	logTime     bool
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet("goslimplayer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: goslimplayer [options]

  goslimplayer -server 192.168.2.20             play on this PC's sound device
  goslimplayer -u config.json                   present the renderers in the file to LMS and stream to them
  goslimplayer -l                               list the UPnP renderers on the network
  goslimplayer -jsonfile                        list them and write a config.json skeleton

Options:
`)
		fs.PrintDefaults()
	}

	// Slimproto side
	fs.StringVar(&f.server, "server", "", "LMS host[:port]; found by broadcast if empty (a server entry in the config file also works)")
	fs.StringVar(&f.name, "name", "", "local playback: player name (default: host name plus a random number 1-999; LMS can override)")
	fs.StringVar(&f.mac, "mac", "", "local playback: player MAC, e.g. 02:00:47:6f:54:31")
	fs.IntVar(&f.rate, "rate", 48000, "output sample rate in Hz (other rates from LMS are resampled; 48000 suits Sonos)")
	fs.IntVar(&f.maxRate, "maxrate", 0, "highest sample rate to accept from LMS, in Hz (0 = same as -rate); anything above -rate is resampled here")
	fs.IntVar(&f.webPort, "webport", 9000, "LMS web/JSON-RPC port, used to look up track info for the log (0 = off)")
	fs.StringVar(&f.webUser, "webuser", "", "LMS web interface user name, if it is password protected")
	fs.StringVar(&f.webPass, "webpass", "", "LMS web interface password")
	fs.BoolVar(&f.replayGain, "replaygain", true, "apply the ReplayGain adjustment LMS asks for (when enabled in the player's LMS settings)")
	fs.BoolVar(&f.nativeALAC, "nativealac", true, "default: decode ALAC (Apple Lossless, 16 and 24-bit, mono or stereo) with the built-in decoder. false turns it off: ALAC then goes to\n-ffmpeg if given, and otherwise LMS transcodes ALAC files itself")
	fs.BoolVar(&f.nativeOgg, "nativevorbis", true, "default: decode Ogg Vorbis with the built-in decoder. false turns it off and LMS transcodes Ogg Vorbis files itself")
	fs.BoolVar(&f.nativeAAC, "nativeaac", true, "default: decode AAC-LC (ADTS .aac and MP4/M4A, mono or stereo) with the built-in decoder. false turns it off: AAC then goes to\n-ffmpeg if given, and otherwise LMS transcodes AAC files itself (use this if HE-AAC files cause trouble)")
	fs.StringVar(&f.ffmpeg, "ffmpeg", "", "path to ffmpeg: lets GoSlimPlayer decode ALAC, and the AAC the built-in decoder cannot (HE-AAC, LATM), from LMS itself (experimental);\nwithout it LMS transcodes such files to FLAC/MP3")

	// UPnP side
	fs.StringVar(&f.configFile, "u", "", "UPnP mode: JSON config file with an optional server entry (LMS host[:port]) and a players list to drive:\neach entry needs a name (may be empty to use the renderer's own name) and a mac; optional volume_offset, volume_max,\nbuffer_seconds, http_port, enabled")
	fs.BoolVar(&f.list, "l", false, "list the UPnP audio renderers on the network and exit")
	fs.BoolVar(&f.asJSON, "json", false, "with -l: also print a config.json skeleton")
	fs.Var(&f.jsonFile, "jsonfile", "list the renderers and write a config.json skeleton to the current directory\n(-jsonfile=name.json for another name; never overwrites an existing file)")
	fs.BoolVar(&f.info, "info", false, "with -m: print a renderer's zone group / stereo pair and per-channel volume, then exit")
	fs.StringVar(&f.infoMAC, "m", "", "renderer MAC address for -info")
	timeout := fs.Int("t", 4, "renderer discovery timeout in seconds")
	bind := fs.String("bind", "", "local IPv4 address to run renderer discovery from (useful with several adapters)")
	fs.IntVar(&f.httpStartPort, "httpstartport", 0, "first local TCP port for the audio streams served to the renderers: with 30000, player 1 serves on\n30000, player 2 on 30001 and so on, in config-file order (0 = any free port; a player's http_port in the file overrides this)")
	fs.Int64Var(&f.clen, "clen", 0, "advertise this Content-Length for the WAV streams (0 = chunked)")
	idle := fs.Int("idle", 5, "seconds without audio from LMS before a renderer stream is ended")
	fs.IntVar(&f.volOffset, "voloffset", 0, "default: add this to the volume LMS asks for, in renderer percent")
	fs.IntVar(&f.volMax, "volmax", 100, "default: never set a renderer louder than this percent")
	fs.Float64Var(&f.buffer, "buffer", 0, "default: seconds of audio a renderer may hold ahead of what it is playing (min 3).\n0 = no limit, the renderer decides (a Sonos can buffer well over a minute, so controllers run far ahead of the sound);\n10-20 keeps LMS and controllers close to the audio")
	fs.BoolVar(&f.decodeAudio, "decodeaudio", true, "default: decode LMS's audio and send the renderer PCM. false hands LMS's original MP3/FLAC/WAV/AIFF to the\nrenderer untouched (experimental: no ReplayGain, no gapless, a short gap between tracks); implies -resampleaudio=false")
	fs.BoolVar(&f.resampleAudio, "resampleaudio", true, "default: resample everything to -rate. false sends each track's decoded PCM at its own sample rate\n(a change of rate starts a new stream with a short gap)")
	fs.BoolVar(&f.icy, "icy", false, "default: send track title and cover art to renderers that ask for ICY in-band metadata (Icy-Metadata: 1).\nOff by default: a renderer that asks but does not apply it to the stream format may play the metadata as noise")
	fs.BoolVar(&f.autoResume, "autoresume", false, "default: when a renderer stops pulling the stream unexpectedly (Wi-Fi drop, renderer reset), start a new stream\ninstead of pausing LMS; pressing stop in LMS still stops")
	fs.BoolVar(&f.exitOnNetFail, "exitonnetfail", false, "default: log it and carry on. true exits with an error when a renderer never fetches its audio stream (firewall\nblocking the stream port, wrong network after a reconfiguration), so a service manager such as systemd restarts the program until it works")
	fs.DurationVar(&f.probeWindow, "reconnectreplay", upnp.ProbeWindow, "a renderer that opens a stream, closes it and opens it again (GStreamer-based ones do) gets the stream from\nthe start again if it comes back within this time of its first request, however much it had read. 0 turns this off")
	fs.BoolVar(&f.verbose, "v", false, "verbose output: log what each track is, LMS volume changes and renderer statistics")

	// process
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
