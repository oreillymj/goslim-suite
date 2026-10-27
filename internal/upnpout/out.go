// Package upnpout feeds one Slimproto player's audio to a UPnP renderer.
//
// The player's track queue is drained by a pump into a stream that the
// renderer pulls over HTTP. In the normal mode the stream is a WAV of decoded
// PCM, which keeps tracks gapless; in passthrough mode LMS's original MP3,
// FLAC, WAV or AIFF is served untouched, one URL per track. The renderer's own
// state (what it has pulled, pauses, stops, disappearing) feeds back into what
// the player tells LMS.
package upnpout

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"goslim/internal/player"
	"goslim/internal/upnp"
)

const (
	feedChunk  = 16 << 10
	feedWindow = 2 << 20 // max PCM bytes handed over beyond what the renderer has pulled (~11 s)

	// A WAV stream can't be longer than 4 GiB: its length field is 32 bits and
	// a renderer stops when it has played that much. Streams are replaced at the
	// next track start after RotateBytes, or mid-track after the slack.
	defaultRotate = 3 << 30
	rotateSlack   = 256 << 20
)

// Options configure an Out.
type Options struct {
	Name              string
	Clen              int64 // Content-Length to advertise; 0 = chunked
	Idle              time.Duration
	VolOffset, VolMax int
	BufferSecs        float64             // >0: pace the stream so the renderer holds at most this much ahead of playback
	AutoResume        bool                // start a new stream if the renderer stops pulling one
	ICY               bool                // answer Icy-Metadata requests with track title and cover art in the stream
	RotateBytes       int64               // replace the stream after this many bytes (0 = 3 GiB)
	PortScan          int                 // ports above the original to try if the HTTP port must be re-bound (0 = 50)
	OnNetFail         func(reason string) // called when the renderer cannot reach our stream at all (firewall, wrong network); nil = just log
}

// trackMark remembers where in the byte stream a track begins, so its
// transition can be logged when the renderer actually reaches it.
type trackMark struct {
	at     int64
	queued time.Time
	info   <-chan *player.TrackInfo
}

// Out is the output of one player. It implements player.Output and http.Handler.
type Out struct {
	c    *player.Client
	name string
	up   *upnp.Client

	clen              int64
	idle              time.Duration
	volOffset, volMax int
	bufferSecs        float64
	window            int64
	autoResume        bool
	icy               bool
	rotateBytes       int64
	portScan          int
	onNetFail         func(reason string)
	raw               bool // passthrough mode

	cmds chan func() // serialises SOAP calls so the pump never waits on the renderer
	kick chan struct{}

	sent       atomic.Int64 // bytes handed to streams (cumulative)
	consumed   atomic.Int64 // bytes the renderer pulled, or that were dropped (cumulative)
	flushGen   atomic.Int64 // bumped on every flush so stale audio and track marks are dropped
	pendingEnd atomic.Bool  // queue ran dry; report STMu once the renderer has caught up
	gone       atomic.Bool  // the renderer's stream ended on its side
	recover    atomic.Bool  // the stream was lost: start a new one as soon as the renderer answers
	port       atomic.Int32 // the HTTP port we serve on

	feedMu sync.Mutex // keeps feeding and flushing from interleaving

	mu        sync.Mutex
	cur       *upnp.Stream
	paused    bool // we asked the renderer to pause, or it was paused from its side
	extPaused bool // ... specifically from its side
	lastData  time.Time
	cmdAt     time.Time // when we last sent the renderer a command
	nextID    int
	srv       *http.Server
	lastIP    net.IP

	// what is kept about lost streams (see restarts.go)
	history     []restart
	lost        int // streams lost
	resumes     int // ... of which autoresume restarted
	lostAt      time.Time
	lastState   string       // last renderer transport state the poller saw
	streamBegan time.Time    // when the current stream was started
	lastPull    atomic.Int64 // unix nanos: when the renderer last pulled audio
	tracks      atomic.Int64
	lastMeta    *upnp.TrackMeta // the newest track's metadata; a replacement stream starts with it
	closeOnce   sync.Once

	clockMu       sync.Mutex // wall clock of the current passthrough track
	clockStart    time.Time
	clockPausedAt time.Time
	clockPaused   time.Duration
}

// New creates the output that feeds player c's audio to renderer r. Install it
// with c.SetOutput, give it a listener with Serve, then call Run.
func New(c *player.Client, r *upnp.Renderer, o Options) *Out {
	if o.BufferSecs > 0 && o.BufferSecs < 3 {
		log.Printf("[%s] a buffer of %.1fs is too small to be reliable, using 3s", o.Name, o.BufferSecs)
		o.BufferSecs = 3
	}
	if o.RotateBytes <= 0 {
		o.RotateBytes = defaultRotate
	}
	if o.PortScan <= 0 {
		o.PortScan = 50
	}
	out := &Out{
		c: c, name: o.Name, up: upnp.NewClient(r, o.Name),
		clen: o.Clen, idle: o.Idle, volOffset: o.VolOffset, volMax: o.VolMax,
		bufferSecs: o.BufferSecs, window: feedWindow, autoResume: o.AutoResume, icy: o.ICY,
		rotateBytes: o.RotateBytes, portScan: o.PortScan, onNetFail: o.OnNetFail, raw: c.Options().Passthrough,
		cmds: make(chan func(), 64), kick: make(chan struct{}, 1),
	}
	if o.BufferSecs > 0 {
		out.window = int64(2 * c.OutRate() * 4) // two seconds is plenty when the stream is paced
	}
	go func() {
		for f := range out.cmds {
			f()
		}
	}()
	return out
}

// Run starts the background workers: the pump that feeds the renderer, the
// idle watchdog and the transport-state poll. They stop when the player closes.
func (o *Out) Run() {
	if o.raw {
		go o.pumpRaw()
	} else {
		go o.pumpPCM()
		go o.watchdog()
	}
	go o.poll()
}

// ---------------------------------------------------------------- plumbing

func (o *Out) touch() {
	o.mu.Lock()
	o.cmdAt = time.Now()
	o.mu.Unlock()
}

func (o *Out) do(f func()) {
	o.touch()
	select {
	case o.cmds <- func() { f(); o.touch() }:
	default:
		log.Printf("[%s] command queue full, dropping a renderer command", o.name)
	}
}

func (o *Out) kickNow() {
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

// wait sleeps until there may be something to do: more audio from the player,
// progress by the renderer, or a second has passed.
func (o *Out) wait() {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-o.c.Done():
	case <-o.c.Wake():
	case <-o.kick:
	case <-t.C:
	}
}

// sleep waits d and reports false if the player closed meanwhile.
func (o *Out) sleep(d time.Duration) bool {
	select {
	case <-o.c.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// baseURL is where the renderer can reach us. The local address is looked up
// each time because a DHCP renewal or a network recovery can change it.
func (o *Out) baseURL() string {
	ip, err := upnp.LocalIPFor(o.up.R.IP)
	o.mu.Lock()
	if err == nil {
		o.lastIP = ip
	} else {
		ip = o.lastIP
	}
	o.mu.Unlock()
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(o.port.Load())))
}

// ---------------------------------------------------------------- player.Output

func (o *Out) Inflight() int64 {
	if n := o.sent.Load() - o.consumed.Load(); n > 0 {
		return n
	}
	return 0
}

func (o *Out) Flushed() {
	log.Printf("[%s] stop/flush", o.name)
	o.feedMu.Lock()
	o.flushGen.Add(1)
	o.pendingEnd.Store(false)
	o.recover.Store(false)
	o.gone.Store(false)
	o.clockReset()
	o.stopStream()
	o.feedMu.Unlock()
	o.kickNow()
}

// stopStream ends the current stream at once and tells the renderer to stop.
func (o *Out) stopStream() {
	o.mu.Lock()
	st := o.cur
	o.cur, o.paused, o.extPaused = nil, false, false
	o.mu.Unlock()
	if st != nil {
		o.consumed.Add(st.Finish(true))
		o.do(func() { o.up.Stop() })
	}
}

func (o *Out) Pause() {
	log.Printf("[%s] pause", o.name)
	o.mu.Lock()
	st := o.cur
	o.paused, o.extPaused = true, false
	o.mu.Unlock()
	if st != nil {
		if st.Pace != nil {
			st.Pace.Drain()
		}
		o.do(func() { o.up.Pause() })
	}
	o.clockPause()
}

func (o *Out) Resume() {
	log.Printf("[%s] resume", o.name)
	o.mu.Lock()
	active := o.cur != nil
	o.paused, o.extPaused = false, false
	o.lastData = time.Now()
	o.mu.Unlock()
	if active {
		o.do(func() { o.up.Play() })
	}
	o.clockResume()
	o.kickNow()
	// If the stream is gone, the next audio from the queue starts a new one.
}

// SetVolume converts LMS's linear gain (1.0 = full scale) to a 0-100 renderer
// volume with a dB mapping (0 dB = 100, -50 dB = 0), then applies the
// per-player offset and cap. In verbose mode the log line also shows LMS's own
// volume setting.
func (o *Out) SetVolume(gain float64) {
	pct := 0.0
	if gain > 0 {
		pct = math.Max(0, math.Min(100, 100+40*math.Log10(gain)))
	}
	v := int(math.Round(pct)) + o.volOffset
	if v > o.volMax {
		v = o.volMax
	}
	if v < 0 {
		v = 0
	}
	o.do(func() {
		lms := ""
		if upnp.Verbose {
			if lv, err := o.c.LMSVolume(); err == nil {
				if lv < 0 {
					lms = fmt.Sprintf("LMS muted (volume %d%%), ", -lv)
				} else {
					lms = fmt.Sprintf("LMS volume %d%%, ", lv)
				}
			}
		}
		if err := o.up.SetVolume(v); err != nil {
			log.Printf("[%s] UPnP SetVolume %d%% failed: %v", o.name, v, err)
			return
		}
		if upnp.Verbose {
			log.Printf("[%s] UPnP SetVolume %d%% ok (%sLMS gain %.3f)", o.name, v, lms, gain)
		}
	})
}

func (o *Out) SetName(name string) {
	log.Printf("[%s] LMS renamed the player to %q", o.name, name)
}

// Elapsed is how far into the current track the renderer is. Only passthrough
// knows (by the wall clock); PCM mode derives it from the bytes served.
func (o *Out) Elapsed() (time.Duration, bool) {
	if !o.raw {
		return 0, false
	}
	o.clockMu.Lock()
	defer o.clockMu.Unlock()
	if o.clockStart.IsZero() {
		return 0, true
	}
	end := time.Now()
	if !o.clockPausedAt.IsZero() {
		end = o.clockPausedAt
	}
	return end.Sub(o.clockStart) - o.clockPaused, true
}

func (o *Out) clockReset() {
	o.clockMu.Lock()
	o.clockStart, o.clockPausedAt, o.clockPaused = time.Time{}, time.Time{}, 0
	o.clockMu.Unlock()
}

func (o *Out) clockPause() {
	o.clockMu.Lock()
	if !o.clockStart.IsZero() && o.clockPausedAt.IsZero() {
		o.clockPausedAt = time.Now()
	}
	o.clockMu.Unlock()
}

func (o *Out) clockResume() {
	o.clockMu.Lock()
	if !o.clockPausedAt.IsZero() {
		o.clockPaused += time.Since(o.clockPausedAt)
		o.clockPausedAt = time.Time{}
	}
	o.clockMu.Unlock()
}

// Close stops serving and waits for queued renderer commands (such as the Stop
// sent when the queue was flushed at shutdown) to reach the renderer, so a
// speaker doesn't keep playing after the program exits.
func (o *Out) Close() {
	o.closeOnce.Do(o.logRestartSummary)
	o.mu.Lock()
	srv := o.srv
	o.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	done := make(chan struct{})
	select {
	case o.cmds <- func() { close(done) }:
	default:
		return
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Printf("[%s] timed out waiting for the renderer to stop", o.name)
	}
}

// ---------------------------------------------------------------- streams

// begin starts a new stream and tells the renderer to play it. rate is the
// PCM sample rate; mime and ext describe a passthrough stream (empty = WAV).
func (o *Out) begin(rate int, mime, ext string) *upnp.Stream {
	var hdr []byte
	if !o.raw {
		hdr = upnp.WAVHeader(rate)
	}
	o.mu.Lock()
	o.nextID++
	st := upnp.NewStream(o.nextID, upnp.WAVFmt{Rate: uint32(rate), Channels: 2, Bits: 16}, hdr)
	st.ContentType = mime
	if o.bufferSecs > 0 && !o.raw {
		byteRate := float64(rate * 4)
		st.Pace = upnp.NewPacer(byteRate, o.bufferSecs*byteRate)
	}
	o.cur, o.paused, o.extPaused, o.lastData = st, false, false, time.Now()
	o.streamBegan = o.lastData
	if o.lastMeta != nil {
		st.AddMeta(0, o.lastMeta) // the track that is still playing when a stream is replaced
	}
	o.mu.Unlock()
	o.armEnd(st, 0, upnp.NeverFetched)
	if ext == "" {
		ext = "wav"
	}
	if mime == "" {
		mime = "audio/wav"
	}

	uri := fmt.Sprintf("%s/stream/%d.%s", o.baseURL(), st.ID, ext)
	if o.raw {
		log.Printf("[%s] starting stream %s (%s, passed through)", o.name, uri, mime)
	} else {
		log.Printf("[%s] starting stream %s (%g kHz PCM)", o.name, uri, float64(rate)/1000)
	}
	o.do(func() {
		if err := o.up.SetURI(uri, upnp.DIDLMime(o.name, uri, mime)); err != nil {
			log.Printf("[%s] SetAVTransportURI: %v", o.name, err)
			return
		}
		if err := o.up.Play(); err != nil {
			log.Printf("[%s] Play: %v", o.name, err)
		}
	})
	return st
}

// armEnd ends the stream if no renderer connection exists d from now (and none
// has been opened in between).
func (o *Out) armEnd(st *upnp.Stream, gen int, d time.Duration) {
	time.AfterFunc(d, func() {
		conns, curGen, ever := st.ConnState()
		if conns != 0 || curGen != gen {
			return
		}
		endNote := ""
		if ever {
			state := "unknown"
			if ts, err := o.up.TransportState(); err == nil {
				state = ts
			} else {
				state = "unreachable: " + err.Error()
			}
			log.Printf("[%s] renderer has disconnected from stream %d (%s served, transport state: %s)",
				o.name, st.ID, upnp.HumanBytes(st.Served()), state)
			endNote = "transport state " + state
		} else {
			endNote = "never fetched"
			log.Printf("[%s] renderer never fetched stream %d (firewall? wrong network?)", o.name, st.ID)
			if o.onNetFail != nil {
				o.onNetFail(fmt.Sprintf("renderer never fetched stream %d (firewall? wrong network?)", st.ID))
			}
		}
		o.endStream(st, "renderer disconnected ("+endNote+")")
	})
}

// endStream handles the renderer's side of a stream going away.
func (o *Out) endStream(st *upnp.Stream, reason string) {
	complete := st.Finished() && st.Drained() // finished on purpose and fully served: it simply ended
	o.mu.Lock()
	was := o.cur == st
	if was && !complete {
		o.cur, o.paused, o.extPaused = nil, false, false
	}
	o.mu.Unlock()
	o.consumed.Add(st.Finish(true))
	if was && !complete {
		o.recordRestart(st, reason)
		o.gone.Store(true)
		o.kickNow()
	}
}

// handleGone reacts to the renderer's stream ending unexpectedly.
func (o *Out) handleGone() {
	if o.autoResume {
		o.recover.Store(true)
		log.Printf("[%s] the renderer stopped pulling the stream; starting a new one when it answers (autoresume)", o.name)
		return
	}
	if o.c.PauseFromOutput() {
		log.Printf("[%s] renderer stopped; pausing", o.name)
	}
}

// waitReachable blocks until the renderer answers a status request, so a new
// stream isn't started (and audio thrown away) while it is off the network. It
// reports false if the queue was flushed or the player closed meanwhile.
func (o *Out) waitReachable(gen int64) bool {
	var lastLog time.Time
	logged := false
	for {
		_, err := o.up.TransportState()
		if err == nil {
			o.recover.Store(false)
			o.noteRecovered()
			if logged {
				log.Printf("[%s] the renderer answers again; resuming playback", o.name)
			}
			return true
		}
		if time.Since(lastLog) > 30*time.Second {
			log.Printf("[%s] waiting for the renderer to answer again: %v", o.name, err)
			lastLog, logged = time.Now(), true
		}
		if !o.sleep(3*time.Second) || o.flushGen.Load() != gen {
			return false
		}
	}
}

// ---------------------------------------------------------------- HTTP

// Serve starts answering the renderer's requests on ln. If the server ever
// stops on its own it listens again, on the same port or the next free one.
func (o *Out) Serve(ln net.Listener) {
	o.port.Store(int32(ln.Addr().(*net.TCPAddr).Port))
	go o.serveLoop(ln)
}

func (o *Out) serveLoop(ln net.Listener) {
	for {
		srv := &http.Server{Handler: o}
		o.mu.Lock()
		o.srv = srv
		o.mu.Unlock()
		err := srv.Serve(ln)
		select {
		case <-o.c.Done():
			return
		default:
		}
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Printf("[%s] HTTP server on port %d stopped: %v; listening again", o.name, o.port.Load(), err)
		if ln = o.relisten(); ln == nil {
			return
		}
	}
}

func (o *Out) relisten() net.Listener {
	want := int(o.port.Load())
	for {
		if !o.sleep(2 * time.Second) {
			return nil
		}
		for p := want; p <= want+o.portScan; p++ {
			ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
			if err != nil {
				continue
			}
			if p != want {
				log.Printf("[%s] port %d is in use by another program; serving on port %d instead", o.name, want, p)
			}
			o.port.Store(int32(p))
			return ln
		}
		log.Printf("[%s] cannot listen on any port from %d to %d; retrying", o.name, want, want+o.portScan)
	}
}

// ServeHTTP answers the renderer's requests for /stream/<id>.<ext>.
func (o *Out) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var id int
	if _, err := fmt.Sscanf(r.URL.Path, "/stream/%d.", &id); err != nil {
		log.Printf("[%s] renderer request %s %s from %s: unknown path", o.name, r.Method, r.URL.Path, r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	o.mu.Lock()
	st := o.cur
	o.mu.Unlock()
	if st == nil || st.ID != id {
		log.Printf("[%s] renderer request %s %s from %s: no such stream", o.name, r.Method, r.URL.Path, r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	if o.raw && r.Method != http.MethodHead {
		if _, _, ever := st.ConnState(); !ever {
			o.clockMu.Lock() // the renderer has started the track
			o.clockStart = time.Now()
			o.clockMu.Unlock()
		}
	}
	st.Serve(w, r, upnp.ServeHooks{
		Name:     o.name,
		Clen:     o.clen,
		ICY:      o.icy,
		Consumed: func(n int64) { o.consumed.Add(n); o.lastPull.Store(time.Now().UnixNano()); o.kickNow() },
		Idle:     func(gen int) { o.armEnd(st, gen, upnp.ReconnectGrace) },
	})
}

// markTrack records for ICY metadata that a track starts at offset at (PCM bytes,
// header excluded) of stream st, and fills in its title and cover art when LMS has
// answered. It returns a channel that yields the same answer, for logging.
func (o *Out) markTrack(st *upnp.Stream, at int64, info <-chan *player.TrackInfo) <-chan *player.TrackInfo {
	if !o.icy {
		return info
	}
	tm := upnp.NewTrackMeta()
	o.mu.Lock()
	o.lastMeta = tm
	o.mu.Unlock()
	st.AddMeta(at, tm)
	out := make(chan *player.TrackInfo, 1)
	go func() {
		ti := <-info
		if ti != nil {
			tm.Set(ti.ICY())
		} else {
			tm.Set("", "") // nothing known: stop anyone waiting
		}
		out <- ti
	}()
	return out
}
