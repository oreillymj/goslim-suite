package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"goslim/internal/config"
	"goslim/internal/decode"
	"goslim/internal/lms"
	"goslim/internal/player"
	"goslim/internal/upnp"
	"goslim/internal/upnpout"
)

// run does what the command line asked for and returns when ctx is cancelled
// (or immediately for the one-shot modes).
func run(ctx context.Context, f *flags) error {
	upnp.Verbose = f.verbose
	switch {
	case f.info:
		return showInfo(f)
	case f.list || f.jsonFile.IsSet():
		return listRenderers(f)
	case f.configFile != "":
		return playUPnP(ctx, f)
	}
	return playLocal(ctx, f)
}

// playerOptions are the settings common to every player this command creates.
func playerOptions(f *flags, finder *lms.Finder, name string, mac net.HardwareAddr) (player.Options, error) {
	dec := &decode.Decoder{OutRate: f.rate, NoNativeAAC: !f.nativeAAC, NoNativeALAC: !f.nativeALAC, NoNativeVorbis: !f.nativeOgg}
	if f.ffmpeg != "" {
		p, err := decode.FindFFmpeg(f.ffmpeg)
		if err != nil {
			return player.Options{}, fmt.Errorf("-ffmpeg: %w", err)
		}
		dec.FFmpeg = p
	}
	return player.Options{
		Name: name, MAC: mac, OutRate: f.rate, MaxRate: f.maxRate, Decoder: dec, Finder: finder,
		ReplayGain: f.replayGain, Verbose: f.verbose, Web: player.WebConfig{Port: f.webPort, User: f.webUser, Pass: f.webPass},
	}, nil
}

// ---------------------------------------------------------------- one-shot modes

func showInfo(f *flags) error {
	want := upnp.NormMAC(f.infoMAC)
	if want == "" {
		return errors.New("-info needs -m <renderer MAC address>")
	}
	rs := upnp.DiscoverRenderers(f.bind, f.timeout)
	for _, r := range rs {
		if r.MAC == want {
			upnp.PrintInfo(upnp.NewClient(r, r.Name))
			return nil
		}
	}
	fmt.Fprintf(os.Stderr, "No renderer with MAC %s found. Available renderers:\n", want)
	upnp.PrintRenderers(rs)
	return errors.New("renderer not found")
}

func listRenderers(f *flags) error {
	rs := upnp.DiscoverRenderers(f.bind, f.timeout)
	upnp.PrintRenderers(rs)
	defaults := config.Defaults{VolumeOffset: f.volOffset, VolumeMax: f.volMax, BufferSeconds: f.buffer, HTTPStartPort: f.httpStartPort,
		DecodeAudio: f.decodeAudio, ResampleAudio: f.resampleAudio, AutoResume: f.autoResume, ICY: f.icy}
	if f.asJSON || f.jsonFile.IsSet() {
		defaults.Server = lms.ForConfig(f.server)
	}
	if f.asJSON {
		b, _ := config.Generate(rs, defaults)
		fmt.Printf("\nconfig.json skeleton (for a Sonos stereo pair keep only the coordinator, see -info):\n%s", b)
	}
	if f.jsonFile.IsSet() {
		if err := config.Write(f.jsonFile.Val, rs, defaults); err != nil {
			return fmt.Errorf("-jsonfile: %w", err)
		}
		fmt.Printf("\nWrote %s. Edit the names, and for a Sonos stereo pair keep only the coordinator (see -info).\n", f.jsonFile.Val)
	}
	return nil
}

// ---------------------------------------------------------------- local playback

func playLocal(ctx context.Context, f *flags) error {
	finder := &lms.Finder{}
	finder.SetServer(f.server, f.server != "")
	name := f.name
	if name == "" {
		name = player.DefaultName()
	}
	opts, err := playerOptions(f, finder, name, player.PickMAC(f.mac, 0xa5))
	if err != nil {
		return err
	}
	if !f.decodeAudio || !f.resampleAudio {
		log.Printf("-decodeaudio=false and -resampleaudio=false only apply to UPnP renderers; local playback always decodes and resamples to -rate")
	}
	c := player.New(opts)
	closer, err := c.PlayLocal()
	if err != nil {
		return fmt.Errorf("audio init: %w", err)
	}
	defer closer.Close()
	log.Printf("formats offered to LMS: %s", opts.Decoder.Caps())
	log.Printf("player %q, MAC %s, output %d Hz", c.Name(), c.MAC(), f.rate)
	go c.Loop()
	<-ctx.Done()
	c.Close()
	return nil
}

// ---------------------------------------------------------------- UPnP playback

// upnpPlayer is one running player.
type upnpPlayer struct {
	client *player.Client
}

// portScan is how many ports above -httpstartport may be tried when one is taken.
const portScan = 50

// listenFrom listens on port, or, when scan > 0 and it is taken, on the next free
// port up to port+scan. A port of 0 means any free port.
func listenFrom(port, scan int) (net.Listener, error) {
	var first error
	for p := port; p <= port+scan; p++ {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			if p != port {
				log.Printf("port %d is in use by another program or instance; using port %d instead", port, p)
			}
			return ln, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

func playUPnP(parent context.Context, f *flags) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	players, fileServer, err := config.Load(f.configFile)
	if err != nil {
		return fmt.Errorf("config file %s: %w", f.configFile, err)
	}
	finder := &lms.Finder{}
	finder.SetServer(f.server, f.server != "")
	if finder.Configured() == "" {
		if fileServer != "" {
			finder.SetServer(fileServer, false) // from the file: used until it stops answering
		} else {
			finder.SetHintFile(f.configFile)
		}
	}

	// Open every player's port up front so a clash is dealt with now. A port from
	// the file must be free (it is probably in a firewall rule); a port counted
	// up from -httpstartport moves on to the next free one if taken; otherwise
	// the OS picks.
	lns := make([]net.Listener, len(players))
	for i, pc := range players {
		var ln net.Listener
		var err error
		switch {
		case pc.HTTPPort != nil:
			ln, err = listenFrom(*pc.HTTPPort, 0)
		case f.httpStartPort > 0:
			ln, err = listenFrom(f.httpStartPort+i, portScan)
		default:
			ln, err = listenFrom(0, 0)
		}
		if err != nil {
			label := pc.Name
			if label == "" {
				label = pc.MAC
			}
			for _, l := range lns[:i] {
				l.Close()
			}
			return fmt.Errorf("player %d (%s): cannot listen: %w", i+1, label, err)
		}
		lns[i] = ln
	}
	probe, err := playerOptions(f, finder, "", nil)
	if err != nil {
		return err
	}
	log.Printf("UPnP mode: %d player(s) from %s; formats offered to LMS: %s", len(players), f.configFile, probe.Decoder.Caps())

	renderers := upnp.DiscoverRenderers(f.bind, f.timeout)
	var (
		mu      sync.Mutex
		running []upnpPlayer
		wg      sync.WaitGroup
	)
	for i, pc := range players {
		var found *upnp.Renderer
		for _, r := range renderers {
			if r.MAC == pc.MAC {
				found = r
				break
			}
		}
		wg.Add(1)
		go func(pc config.Player, ln net.Listener, r *upnp.Renderer) {
			defer wg.Done()
			p, ok := startPlayer(ctx, cancel, f, finder, pc, ln, r)
			if !ok {
				return
			}
			mu.Lock()
			running = append(running, p)
			mu.Unlock()
			<-ctx.Done()
		}(pc, lns[i], found)
	}

	<-ctx.Done()
	// Leave LMS, stop the renderers, close the HTTP servers.
	mu.Lock()
	for _, p := range running {
		p.client.Close()
	}
	mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && parent.Err() == nil {
		return err // a player gave up on the network: exit with an error so the service manager restarts us
	}
	return nil
}

// startPlayer finds the renderer (searching again every 30 s until it shows up),
// then starts its player and HTTP server. It reports false if ctx was cancelled
// first or the renderer can't be used.
func startPlayer(ctx context.Context, cancel context.CancelCauseFunc, f *flags, finder *lms.Finder, pc config.Player, ln net.Listener, r *upnp.Renderer) (upnpPlayer, bool) {
	label := pc.Name
	if label == "" {
		label = pc.MAC
	}
	for r == nil {
		log.Printf("[%s] renderer %s not found yet, searching again in 30s (check it is on, and that -l lists it)", label, pc.MAC)
		select {
		case <-ctx.Done():
			return upnpPlayer{}, false
		case <-time.After(30 * time.Second):
		}
		for _, cand := range upnp.DiscoverRenderers(f.bind, f.timeout) {
			if cand.MAC == pc.MAC {
				r = cand
				break
			}
		}
	}
	name := pc.Name
	if name == "" { // left empty in the file: use the renderer's room name
		if name = upnp.ShortName(r.Name); name == "" {
			name = r.Name
		}
	}
	if r.IP == nil {
		log.Printf("[%s] cannot use renderer %s: no IP address", name, r.Location)
		return upnpPlayer{}, false
	}
	mac, err := net.ParseMAC(pc.MAC)
	if err != nil {
		log.Printf("[%s] bad MAC %q: %v", name, pc.MAC, err)
		return upnpPlayer{}, false
	}
	vo, vm, buf := f.volOffset, f.volMax, f.buffer
	decodeAudio, resampleAudio, autoResume, icy := f.decodeAudio, f.resampleAudio, f.autoResume, f.icy
	if pc.VolumeOffset != nil {
		vo = *pc.VolumeOffset
	}
	if pc.VolumeMax != nil {
		vm = *pc.VolumeMax
	}
	if pc.Buffer != nil {
		buf = *pc.Buffer
	}
	if pc.DecodeAudio != nil {
		decodeAudio = *pc.DecodeAudio
	}
	if pc.ResampleAudio != nil {
		resampleAudio = *pc.ResampleAudio
	}
	if pc.AutoResume != nil {
		autoResume = *pc.AutoResume
	}
	if pc.ICY != nil {
		icy = *pc.ICY
	}

	opts, err := playerOptions(f, finder, name, mac)
	if err != nil {
		log.Printf("[%s] %v", name, err)
		return upnpPlayer{}, false
	}
	opts.Passthrough = !decodeAudio
	opts.NoResample = !resampleAudio || !decodeAudio
	c := player.New(opts)
	var onNetFail func(string)
	if f.exitOnNetFail {
		onNetFail = func(reason string) {
			log.Printf("[%s] %s: exiting so the service manager can restart the program", name, reason)
			cancel(fmt.Errorf("[%s] %s", name, reason))
		}
	}
	out := upnpout.New(c, r, upnpout.Options{
		OnNetFail: onNetFail, Name: name, Clen: f.clen, Idle: f.idle, VolOffset: vo, VolMax: vm, BufferSecs: buf, AutoResume: autoResume, ICY: icy,
	})
	c.SetOutput(out)
	out.Serve(ln)
	out.Run()
	log.Printf("[%s] renderer %q at %s, MAC %s, serving audio on port %d, volume offset %+d, max %d, buffer %s, %s, autoresume %v, icy metadata %v",
		name, r.Name, r.IP, pc.MAC, ln.Addr().(*net.TCPAddr).Port, vo, vm, bufferText(buf), audioMode(decodeAudio, resampleAudio), autoResume, icy)
	go c.Loop()
	return upnpPlayer{client: c}, true
}

func audioMode(decode, resample bool) string {
	switch {
	case !decode:
		return "passthrough (undecoded)"
	case !resample:
		return "decoded, native sample rates"
	}
	return "decoded and resampled"
}

func bufferText(secs float64) string {
	if secs <= 0 {
		return "unlimited"
	}
	return strconv.FormatFloat(secs, 'f', -1, 64) + "s"
}
