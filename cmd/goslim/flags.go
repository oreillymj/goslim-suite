package main

import (
	"flag"
	"fmt"
	"os"
)

// flags holds the parsed command line.
type flags struct {
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
	proxyPort  int
	mdns       bool
	mdnsName   string
	stereoTest string
	verbose    bool

	showVersion bool
	logFile     string
	logTime     bool
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet("goslim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: goslim [options]

  goslim -server 192.168.2.20       play on this PC's sound device
  goslim -proxyport 3434            wait for GoUPnPAudioClient connections; each becomes a player in LMS

Options:
`)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.server, "server", "", "LMS host[:port]; found by broadcast if empty")
	fs.StringVar(&f.name, "name", "", "local playback: player name (default: host name plus a random number 1-999; LMS can override)")
	fs.StringVar(&f.mac, "mac", "", "local playback: player MAC, e.g. 02:00:47:6f:53:6c")
	fs.IntVar(&f.rate, "rate", 48000, "output sample rate in Hz (other rates from LMS are resampled; 48000 suits Sonos)")
	fs.IntVar(&f.maxRate, "maxrate", 0, "highest sample rate to accept from LMS, in Hz (0 = same as -rate); anything above -rate is resampled here")
	fs.IntVar(&f.webPort, "webport", 9000, "LMS web/JSON-RPC port, used to look up track info for the log (0 = off)")
	fs.StringVar(&f.webUser, "webuser", "", "LMS web interface user name, if it is password protected")
	fs.StringVar(&f.webPass, "webpass", "", "LMS web interface password")
	fs.BoolVar(&f.replayGain, "replaygain", true, "apply the ReplayGain adjustment LMS asks for (when enabled in the player's LMS settings)")
	fs.BoolVar(&f.nativeALAC, "nativealac", true, "default: decode ALAC (Apple Lossless, 16 and 24-bit, mono or stereo) with the built-in decoder. false turns it off: ALAC then goes to\n-ffmpeg if given, and otherwise LMS transcodes ALAC files itself")
	fs.BoolVar(&f.nativeOgg, "nativevorbis", true, "default: decode Ogg Vorbis with the built-in decoder. false turns it off and LMS transcodes Ogg Vorbis files itself")
	fs.BoolVar(&f.nativeAAC, "nativeaac", true, "default: decode AAC-LC (ADTS .aac and MP4/M4A, mono or stereo) with the built-in decoder. false turns it off: AAC then goes to\n-ffmpeg if given, and otherwise LMS transcodes AAC files itself (use this if HE-AAC files cause trouble)")
	fs.StringVar(&f.ffmpeg, "ffmpeg", "", "path to ffmpeg: lets GoSlim decode ALAC, and the AAC the built-in decoder cannot (HE-AAC, LATM), from LMS itself (experimental);\nwithout it LMS transcodes such files to FLAC/MP3")
	fs.IntVar(&f.proxyPort, "proxyport", 0, "UPnP proxy mode: listen on this TCP port (e.g. 3434) for GoUPnPAudioClient\nconnections instead of playing locally; each connection is its own player. If the port is taken, any free port\nis used instead (announced with mDNS, see -mdns); -1 = always pick a free port")
	fs.BoolVar(&f.mdns, "mdns", true, "proxy mode: announce the proxy port with mDNS so that GoUPnPAudioClient can find it when it is not the default port.\nWithout it a busy -proxyport is an error")
	fs.StringVar(&f.mdnsName, "mdnsname", "", "proxy mode: name announced with mDNS (default: GoSlim <host name> <port>); GoUPnPAudioClient's -mdnsname picks by it")
	fs.StringVar(&f.stereoTest, "stereotest", "", "proxy mode test: stream this WAV file (looked up next to the executable) to each\nconnecting client instead of using LMS audio; exits if the file is missing")
	fs.BoolVar(&f.verbose, "v", false, "verbose output: log what each track is (format, bitrate, sample rate)")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
	fs.StringVar(&f.logFile, "logfile", "", "also append log output to this file")
	fs.BoolVar(&f.logTime, "logtime", true, "prefix log lines with a timestamp (use -logtime=false under systemd, which adds its own)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}
