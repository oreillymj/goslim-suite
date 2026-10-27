// Package uclient is the engine of GoUPnPAudioClient: it connects to the proxy
// port of a GoSlim, receives that player's decoded audio and plays it on a UPnP
// renderer (or, in test mode, on this PC's sound device).
package uclient

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"goslim/internal/proxyproto"
	"goslim/internal/upnp"
)

// Options configure a Session.
type Options struct {
	Renderer          *upnp.Renderer
	Name              string // player name, used in logs and the stream's title
	BaseURL           string // http://<our address as the renderer sees it>:<port>
	Clen              int64  // Content-Length to advertise; 0 = chunked
	Idle              time.Duration
	VolOffset, VolMax int
}

// Session plays one GoSlim proxy connection on a renderer.
type Session struct {
	o  Options
	up *upnp.Client

	// Discover, if set, is asked for GoSlim's address when connecting to the
	// configured one fails.
	Discover func() (string, error)
	found    string // last address Discover gave that worked

	cmds     chan func() // serialises SOAP calls so the frame reader never blocks on them
	consumed atomic.Int64
	test     *testPlayer // non-nil in test mode

	mu       sync.Mutex
	cur      *upnp.Stream
	paused   bool
	lastData time.Time
	nextID   int

	wmu  sync.Mutex
	conn net.Conn
}

// New creates a session that streams to o.Renderer.
func New(o Options) *Session {
	s := &Session{o: o, up: upnp.NewClient(o.Renderer, o.Name), cmds: make(chan func(), 64)}
	s.start()
	return s
}

// NewTest creates a session that plays on this PC's sound device instead.
func NewTest(name string, idle time.Duration) *Session {
	s := &Session{o: Options{Name: name, Idle: idle}, cmds: make(chan func(), 64), test: newTestPlayer(idle)}
	s.start()
	return s
}

func (s *Session) start() {
	go func() {
		for f := range s.cmds {
			f()
		}
	}()
	go s.watchdog()
	go s.statusLoop()
}

func (s *Session) do(f func()) {
	select {
	case s.cmds <- f:
	default:
		log.Printf("command queue full, dropping a renderer command")
	}
}

func (s *Session) send(t byte, p []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.conn != nil {
		proxyproto.WriteFrame(s.conn, t, p)
	}
}

func (s *Session) state() string {
	if s.test != nil {
		return s.test.state()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.cur == nil:
		return "stopped"
	case s.paused:
		return "paused"
	}
	return "playing"
}

func (s *Session) statusLoop() {
	for range time.Tick(250 * time.Millisecond) {
		consumed := s.consumed.Load()
		if s.test != nil {
			consumed = s.test.consumed.Load()
		}
		b, _ := json.Marshal(proxyproto.Status{State: s.state(), Consumed: consumed})
		s.send(proxyproto.FrameStatus, b)
	}
}

// watchdog ends the HTTP stream when GoSlim stops sending, so the renderer
// plays out its buffer and stops instead of waiting forever.
func (s *Session) watchdog() {
	for range time.Tick(time.Second) {
		s.mu.Lock()
		st := s.cur
		idle := st != nil && !s.paused && st.Drained() && time.Since(s.lastData) > s.o.Idle
		if idle {
			s.cur = nil
		}
		s.mu.Unlock()
		if idle {
			log.Printf("no audio for %s, ending stream", s.o.Idle)
			st.Finish(false)
		}
	}
}

func (s *Session) stop() {
	if s.test != nil {
		s.test.stop()
		return
	}
	s.mu.Lock()
	st := s.cur
	s.cur, s.paused = nil, false
	s.mu.Unlock()
	if st != nil {
		s.consumed.Add(st.Finish(true))
		s.do(func() { s.up.Stop() })
	}
}

// armEnd ends the stream if no renderer connection exists d from now (and none
// has been opened in between).
func (s *Session) armEnd(st *upnp.Stream, gen int, d time.Duration) {
	time.AfterFunc(d, func() {
		conns, cur, ever := st.ConnState()
		if conns != 0 || cur != gen {
			return
		}
		if ever {
			log.Printf("renderer has disconnected from stream %d", st.ID)
		} else {
			log.Printf("renderer never fetched stream %d (firewall? wrong network?)", st.ID)
		}
		s.mu.Lock()
		if s.cur == st {
			s.cur, s.paused = nil, false
		}
		s.mu.Unlock()
		s.consumed.Add(st.Finish(true))
	})
}

func (s *Session) begin(f upnp.WAVFmt, hdr []byte) {
	s.mu.Lock()
	s.nextID++
	st := upnp.NewStream(s.nextID, f, hdr)
	s.cur, s.paused, s.lastData = st, false, time.Now()
	s.mu.Unlock()
	s.armEnd(st, 0, upnp.NeverFetched)

	uri := fmt.Sprintf("%s/stream/%d.wav", s.o.BaseURL, st.ID)
	log.Printf("starting stream %s (%d Hz, %d ch, %d bit)", uri, f.Rate, f.Channels, f.Bits)
	s.do(func() {
		if err := s.up.SetURI(uri, upnp.DIDL(s.o.Name, uri)); err != nil {
			log.Printf("SetAVTransportURI: %v", err)
			return
		}
		if err := s.up.Play(); err != nil {
			log.Printf("Play: %v", err)
		}
	})
}

// ParseHeader reads the format from a 44-byte WAV header.
func ParseHeader(h []byte) (upnp.WAVFmt, bool) {
	if len(h) < 44 || string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return upnp.WAVFmt{}, false
	}
	return upnp.WAVFmt{
		Rate:     binary.LittleEndian.Uint32(h[24:28]),
		Channels: uint32(binary.LittleEndian.Uint16(h[22:24])),
		Bits:     uint32(binary.LittleEndian.Uint16(h[34:36])),
	}, true
}

func (s *Session) onTrack(hdr []byte) {
	f, ok := ParseHeader(hdr)
	if !ok {
		log.Printf("bad WAV header from GoSlim")
		return
	}
	s.mu.Lock()
	st := s.cur
	same := st != nil && st.Fmt == f
	s.mu.Unlock()
	if same {
		return // same format: keep the stream running, the next track is gapless
	}
	if st != nil {
		s.stop()
	}
	s.begin(f, hdr)
}

func (s *Session) onData(p []byte) {
	s.mu.Lock()
	st := s.cur
	s.lastData = time.Now()
	s.mu.Unlock()
	if st == nil || !st.AppendData(p) {
		s.consumed.Add(int64(len(p))) // no stream to play it on: discard
	}
}

func (s *Session) readLoop(conn net.Conn) error {
	for {
		t, p, err := proxyproto.ReadFrame(conn)
		if err != nil {
			return err
		}
		if s.test != nil {
			s.test.handle(t, p)
			continue
		}
		switch t {
		case proxyproto.FrameTrack:
			s.onTrack(p)
		case proxyproto.FrameData:
			s.onData(p)
		case proxyproto.FrameEnd:
		case proxyproto.FrameStop:
			s.stop()
		case proxyproto.FramePause:
			s.mu.Lock()
			st := s.cur
			s.paused = true
			s.mu.Unlock()
			if st != nil {
				if st.Pace != nil {
					st.Pace.Drain()
				}
				s.do(func() { s.up.Pause() })
			}
		case proxyproto.FrameResume:
			s.mu.Lock()
			active := s.cur != nil
			s.paused = false
			s.lastData = time.Now()
			s.mu.Unlock()
			if active {
				s.do(func() { s.up.Play() })
			}
		case proxyproto.FrameVolume:
			if len(p) >= 1 {
				v := int(p[0]) + s.o.VolOffset
				if v > s.o.VolMax {
					v = s.o.VolMax
				}
				if v < 0 {
					v = 0
				}
				if upnp.Verbose {
					log.Printf("volume: LMS %d%% -> renderer %d%%", p[0], v)
				}
				s.do(func() { s.up.SetVolume(v) })
			}
		case proxyproto.FrameName:
			log.Printf("player name is now %q", string(p))
		}
	}
}

// dial connects to addr and, if that fails and Discover is set, to the address it
// finds (an mDNS lookup of GoSlim's proxy). A discovered address that worked is
// tried first next time.
func (s *Session) dial(addr string) (net.Conn, string, error) {
	try := []string{addr}
	if s.found != "" {
		try = []string{s.found, addr}
	}
	var err error
	for _, a := range try {
		var c net.Conn
		if c, err = net.DialTimeout("tcp", a, 5*time.Second); err == nil {
			return c, a, nil
		}
	}
	if s.Discover != nil {
		a, derr := s.Discover()
		if derr != nil {
			log.Printf("connect %s failed and mDNS found nothing: %v", addr, derr)
			return nil, "", err
		}
		c, err2 := net.DialTimeout("tcp", a, 5*time.Second)
		if err2 == nil {
			log.Printf("GoSlim not on %s; found it with mDNS at %s", addr, a)
			s.found = a
			return c, a, nil
		}
		s.found = ""
		return nil, "", err2
	}
	return nil, "", err
}

// ConnectLoop keeps a connection to GoSlim's proxy port alive until stop is
// closed, then leaves the renderer stopped.
func (s *Session) ConnectLoop(addr0 string, hello proxyproto.Hello, stop <-chan struct{}) {
	defer s.shutdown()
	for {
		select {
		case <-stop:
			return
		default:
		}
		conn, addr, err := s.dial(addr0)
		if err != nil {
			log.Printf("connect %s: %v (retrying)", addr0, err)
			if !sleep(stop, 5*time.Second) {
				return
			}
			continue
		}
		log.Printf("connected to GoSlim at %s as %q (%s)", addr, hello.Name, hello.MAC)
		s.consumed.Store(0) // the byte counters are per connection
		if s.test != nil {
			s.test.consumed.Store(0)
		}
		s.wmu.Lock()
		s.conn = conn
		s.wmu.Unlock()
		b, _ := json.Marshal(hello)
		s.send(proxyproto.FrameHello, b)

		done := make(chan struct{})
		go func() { // closing the connection ends readLoop at shutdown
			select {
			case <-stop:
				conn.Close()
			case <-done:
			}
		}()
		err = s.readLoop(conn)
		close(done)
		s.wmu.Lock()
		s.conn = nil
		s.wmu.Unlock()
		conn.Close()
		s.stop()
		select {
		case <-stop:
			return
		default:
		}
		log.Printf("GoSlim connection lost: %v", err)
		if !sleep(stop, 5*time.Second) {
			return
		}
	}
}

func sleep(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// shutdown stops the renderer and waits (briefly) for the command to arrive.
func (s *Session) shutdown() {
	s.stop()
	done := make(chan struct{})
	select {
	case s.cmds <- func() { close(done) }:
	default:
		return
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// ServeHTTP answers the renderer's requests for /stream/<id>.wav.
func (s *Session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var id int
	if _, err := fmt.Sscanf(r.URL.Path, "/stream/%d.wav", &id); err != nil {
		log.Printf("renderer request %s %s from %s: unknown path", r.Method, r.URL.Path, r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	st := s.cur
	s.mu.Unlock()
	if st == nil || st.ID != id {
		log.Printf("renderer request %s %s from %s: no such stream", r.Method, r.URL.Path, r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	st.Serve(w, r, upnp.ServeHooks{
		Name:     s.o.Name,
		Clen:     s.o.Clen,
		Consumed: func(n int64) { s.consumed.Add(n) },
		Idle:     func(gen int) { s.armEnd(st, gen, upnp.ReconnectGrace) },
	})
}
