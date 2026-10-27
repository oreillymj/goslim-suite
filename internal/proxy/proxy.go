// Package proxy is GoSlim's UPnP proxy mode. Instead of playing on the local
// sound device, every GoUPnPAudioClient that connects to the proxy port becomes
// its own Slimproto player (using the renderer's MAC and name), and that
// player's decoded audio is forwarded to the client as WAV/PCM frames (see
// package proxyproto).
package proxy

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"goslim/internal/player"
	"goslim/internal/proxyproto"
	"goslim/internal/upnp"
)

const (
	chunk  = 16 << 10
	window = 2 << 20 // max PCM bytes in flight beyond what the renderer has pulled (~11 s)
	grace  = 3 * time.Second
)

// Server accepts GoUPnPAudioClient connections.
type Server struct {
	Rate       int // PCM sample rate sent to clients
	NewPlayer  func(name string, mac net.HardwareAddr) *player.Client
	StereoTest *StereoTest // if set, stream this file to each client instead of LMS audio

	mu     sync.Mutex
	active map[string]bool // MACs currently connected
	conns  map[net.Conn]struct{}
}

// Run serves clients on ln until ctx is cancelled, then disconnects every client.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	port := ln.Addr().(*net.TCPAddr).Port
	s.active = map[string]bool{}
	s.conns = map[net.Conn]struct{}{}
	log.Printf("proxy mode: waiting for GoUPnPAudioClient on port %d, output %d Hz", port, s.Rate)

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close() // each handler then shuts its player down
		}
		s.mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// conn is the proxy connection of one client. It implements player.Output.
type conn struct {
	nc   net.Conn
	name string
	wmu  sync.Mutex

	sent       atomic.Int64  // PCM bytes written to the client
	consumed   atomic.Int64  // PCM bytes the renderer pulled or the client discarded
	state      atomic.Value  // string: last renderer state reported by the client
	lastT      atomic.Int64  // unix nanos of the last track frame, or of a resume
	needT      atomic.Bool   // send a track frame before the next audio
	pendingEnd atomic.Bool   // queue ran dry; report STMu once the renderer has caught up
	flushGen   atomic.Int64  // bumped on every flush, so the pump can drop stale track marks
	kick       chan struct{} // pulsed when the client reports progress
}

func (p *conn) send(t byte, b []byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := proxyproto.WriteFrame(p.nc, t, b); err != nil {
		p.nc.Close() // makes the reader loop notice and clean up
	}
}

func (p *conn) Inflight() int64 {
	if n := p.sent.Load() - p.consumed.Load(); n > 0 {
		return n
	}
	return 0
}

func (p *conn) Flushed() {
	log.Printf("[%s] proxy: stop/flush", p.name)
	// The renderer is legitimately stopped until the next track frame goes out,
	// so don't judge its state until then.
	p.lastT.Store(0)
	p.flushGen.Add(1)
	p.pendingEnd.Store(false)
	p.send(proxyproto.FrameStop, nil)
}

func (p *conn) Pause() {
	log.Printf("[%s] proxy: pause", p.name)
	p.send(proxyproto.FramePause, nil)
}

func (p *conn) Resume() {
	log.Printf("[%s] proxy: resume", p.name)
	if s, _ := p.state.Load().(string); s == "stopped" {
		p.needT.Store(true) // the renderer stream is gone, ask for a fresh one
		p.lastT.Store(0)    // and don't judge it until that track frame has been sent
	} else {
		p.lastT.Store(time.Now().UnixNano()) // restart the grace period
	}
	p.send(proxyproto.FrameResume, nil)
}

// SetVolume converts LMS's linear gain (1.0 = full scale) to a 0-100 renderer
// volume with a dB mapping: 0 dB = 100, -50 dB = 0.
func (p *conn) SetVolume(gain float64) {
	pct := 0.0
	if gain > 0 {
		pct = math.Max(0, math.Min(100, 100+40*math.Log10(gain)))
	}
	p.send(proxyproto.FrameVolume, []byte{byte(math.Round(pct))})
}

func (p *conn) SetName(name string) { p.send(proxyproto.FrameName, []byte(name)) }

func (p *conn) Close() { p.nc.Close() }

func (p *conn) Elapsed() (time.Duration, bool) { return 0, false }

// wait sleeps until there may be something to do: more audio, more progress
// reported by the client, or a second has passed.
func (p *conn) wait(c *player.Client) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-c.Done():
	case <-c.Wake():
	case <-p.kick:
	case <-t.C:
	}
}

// trackMark remembers where in the byte stream a track begins, so its
// transition can be logged when the renderer actually reaches it.
type trackMark struct {
	at     int64
	queued time.Time
	info   <-chan *player.TrackInfo
}

// pump moves decoded PCM from the player's queue to the proxy connection,
// never getting more than window ahead of what the renderer has consumed.
func pump(c *player.Client, p *conn) {
	hdr := upnp.WAVHeader(c.OutRate())
	buf := make([]byte, chunk)
	wantT := false
	var marks []trackMark
	gen := p.flushGen.Load()
	for {
		select {
		case <-c.Done():
			return
		default:
		}
		if g := p.flushGen.Load(); g != gen {
			gen, marks = g, nil // flushed: those tracks will never be heard
		}
		// A track is audible once the renderer has consumed its first byte, hence >.
		// (After a flush the new track starts at the current consumed count, so >=
		// would fire before the renderer had played anything.)
		if len(marks) > 0 && p.consumed.Load() > marks[0].at {
			m := marks[0]
			marks = marks[1:]
			if s, _ := p.state.Load().(string); s != "stopped" {
				go c.Announce(time.Now(), m.queued, m.info)
			} else {
				marks = nil // renderer is stopped: the bytes were discarded, not played
			}
		}
		// Report "track finished" only once the renderer has pulled everything.
		if p.pendingEnd.Load() && p.consumed.Load() >= p.sent.Load() {
			p.pendingEnd.Store(false)
			log.Printf("[%s] proxy: playback finished", c.Name())
			c.Notify("STMu")
		}
		checkRendererStopped(c, p)
		if p.needT.Swap(false) {
			wantT = true
		}
		if p.Inflight() >= window {
			p.wait(c)
			continue
		}
		r := c.Pull(buf)
		if r.Started {
			wantT = true
		}
		base := p.sent.Load()
		for _, off := range r.Starts {
			marks = append(marks, trackMark{at: base + int64(off), queued: time.Now(), info: c.FetchLater()})
		}
		if r.N > 0 {
			if wantT {
				wantT = false
				p.lastT.Store(time.Now().UnixNano())
				log.Printf("[%s] proxy: sending audio to client (%d Hz)", c.Name(), c.OutRate())
				p.send(proxyproto.FrameTrack, hdr)
			}
			p.sent.Add(int64(r.N))
			p.send(proxyproto.FrameData, buf[:r.N])
		}
		for i := 0; i < r.Ended; i++ {
			p.send(proxyproto.FrameEnd, nil)
		}
		if r.Emptied {
			p.pendingEnd.Store(true)
		}
		if r.N == 0 && r.Ended == 0 && !r.Emptied {
			p.wait(c)
		}
	}
}

// checkRendererStopped handles the renderer being stopped from outside (e.g.
// the Sonos app): without this GoSlim would keep decoding into the void and
// race through the playlist. We pause instead; pressing play resumes.
func checkRendererStopped(c *player.Client, p *conn) {
	if s, _ := p.state.Load().(string); s != "stopped" {
		return
	}
	lt := p.lastT.Load()
	if lt == 0 || time.Since(time.Unix(0, lt)) < grace {
		return
	}
	if c.PauseFromOutput() {
		log.Printf("[%s] renderer stopped; pausing", c.Name())
		p.needT.Store(true)
	}
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	t, payload, err := proxyproto.ReadFrame(nc)
	nc.SetReadDeadline(time.Time{})
	var h proxyproto.Hello
	if err != nil || t != proxyproto.FrameHello || json.Unmarshal(payload, &h) != nil {
		log.Printf("proxy: bad hello from %s", nc.RemoteAddr())
		return
	}
	mac, err := net.ParseMAC(h.MAC)
	if err != nil || len(mac) != 6 {
		log.Printf("proxy: bad MAC %q from %s", h.MAC, nc.RemoteAddr())
		return
	}
	if h.Name == "" {
		h.Name = "GoSlim"
	}

	if s.StereoTest != nil {
		s.StereoTest.serve(nc, h, s.Rate)
		return
	}

	key := mac.String()
	s.mu.Lock()
	if s.active[key] {
		s.mu.Unlock()
		log.Printf("proxy: %s is already connected, rejecting %s", key, nc.RemoteAddr())
		return
	}
	s.active[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()
	}()

	c := s.NewPlayer(h.Name, mac)
	p := &conn{nc: nc, name: h.Name, kick: make(chan struct{}, 1)}
	p.state.Store("stopped")
	c.SetOutput(p)
	log.Printf("proxy: %q (%s) connected from %s", h.Name, key, nc.RemoteAddr())
	go c.Loop()
	go pump(c, p)

	for {
		t, payload, err := proxyproto.ReadFrame(nc)
		if err != nil {
			break
		}
		if t == proxyproto.FrameStatus {
			var st proxyproto.Status
			if json.Unmarshal(payload, &st) == nil {
				p.consumed.Store(st.Consumed) // the client counts from zero on every connection
				select {
				case p.kick <- struct{}{}:
				default:
				}
				if old, _ := p.state.Swap(st.State).(string); old != st.State {
					log.Printf("[%s] renderer state: %s", c.Name(), st.State)
				}
			}
		}
	}
	log.Printf("proxy: %q (%s) disconnected", h.Name, key)
	c.Close()
}
