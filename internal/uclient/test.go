package uclient

import (
	"encoding/binary"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"goslim/internal/audio"
	"goslim/internal/proxyproto"
)

// testPlayer is test mode: instead of handing the stream to a UPnP renderer,
// play the PCM that GoSlim sends on this PC's sound device. It takes the place
// of the renderer in every other respect (status frames, flow control, pause,
// stop, volume), so it proves the LMS -> GoSlim -> GoUPnPAudioClient path on
// its own.
type testPlayer struct {
	idle time.Duration

	mu       sync.Mutex
	buf      []byte
	active   bool // between a track frame and a stop
	paused   bool
	gotData  bool
	volume   float64
	lastData time.Time

	consumed atomic.Int64 // PCM bytes played or discarded (reported to GoSlim)
	opened   bool
	rate     int
}

func newTestPlayer(idle time.Duration) *testPlayer {
	return &testPlayer{idle: idle, volume: 1}
}

func (t *testPlayer) initAudio(rate int) {
	if t.opened {
		if rate != t.rate {
			log.Printf("test: stream rate changed to %d Hz, output stays at %d Hz", rate, t.rate)
		}
		return
	}
	if _, err := audio.Open(rate, t); err != nil {
		log.Printf("test: audio init failed: %v", err)
		return
	}
	t.opened, t.rate = true, rate
	log.Printf("test: audio output open at %d Hz", rate)
}

func (t *testPlayer) state() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active && !t.paused && len(t.buf) == 0 && time.Since(t.lastData) > t.idle {
		t.active = false
		log.Printf("test: no audio for %s, stream ended", t.idle)
	}
	switch {
	case !t.active:
		return "stopped"
	case t.paused:
		return "paused"
	}
	return "playing"
}

func (t *testPlayer) stop() {
	t.mu.Lock()
	t.consumed.Add(int64(len(t.buf)))
	t.buf, t.active, t.paused = nil, false, false
	t.mu.Unlock()
}

func (t *testPlayer) handle(typ byte, p []byte) {
	switch typ {
	case proxyproto.FrameTrack:
		f, ok := ParseHeader(p)
		if !ok || f.Channels != 2 || f.Bits != 16 {
			log.Printf("test: unsupported WAV header from GoSlim (%+v)", f)
			return
		}
		t.initAudio(int(f.Rate))
		t.mu.Lock()
		if !t.active {
			log.Printf("test: stream started (%d Hz, %d ch, %d bit)", f.Rate, f.Channels, f.Bits)
		}
		t.active, t.paused, t.gotData, t.lastData = true, false, false, time.Now()
		t.mu.Unlock()
	case proxyproto.FrameData:
		t.mu.Lock()
		if !t.active {
			t.mu.Unlock()
			t.consumed.Add(int64(len(p))) // arrived after a stop
			return
		}
		t.buf = append(t.buf, p...)
		t.lastData = time.Now()
		first := !t.gotData
		t.gotData = true
		t.mu.Unlock()
		if first {
			log.Printf("test: receiving audio from GoSlim")
		}
	case proxyproto.FrameStop:
		log.Printf("test: stop")
		t.stop()
	case proxyproto.FramePause:
		log.Printf("test: pause")
		t.mu.Lock()
		t.paused = true
		t.mu.Unlock()
	case proxyproto.FrameResume:
		log.Printf("test: resume")
		t.mu.Lock()
		t.paused = false
		t.lastData = time.Now()
		t.mu.Unlock()
	case proxyproto.FrameVolume:
		if len(p) >= 1 {
			log.Printf("test: volume %d%%", p[0])
			t.mu.Lock()
			t.volume = float64(p[0]) / 100
			t.mu.Unlock()
		}
	case proxyproto.FrameName:
		log.Printf("test: player name is now %q", string(p))
	}
}

// Read feeds the sound device: real audio when available, silence otherwise.
func (t *testPlayer) Read(p []byte) (int, error) {
	t.mu.Lock()
	n := 0
	if t.active && !t.paused {
		n = len(t.buf) &^ 3
		if max := len(p) &^ 3; n > max {
			n = max
		}
		copy(p, t.buf[:n])
		t.buf = t.buf[n:]
		t.consumed.Add(int64(n))
	}
	vol := t.volume
	t.mu.Unlock()

	if vol < 0.999 {
		for i := 0; i+1 < n; i += 2 {
			s := int16(binary.LittleEndian.Uint16(p[i:]))
			binary.LittleEndian.PutUint16(p[i:], uint16(int16(float64(s)*vol)))
		}
	}
	for i := n; i < len(p); i++ {
		p[i] = 0
	}
	return len(p), nil
}
