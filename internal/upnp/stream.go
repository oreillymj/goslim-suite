package upnp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	replayLimit = 1 << 20 // a new connection replays the stream from the start only while less than this has been served
	keepServed  = 8 << 20 // already-served audio kept around for such replays
)

// How long a stream waits for a renderer to reconnect, and for its first
// request. Variables so tests can shorten them.
var (
	ReconnectGrace = 5 * time.Second
	NeverFetched   = 10 * time.Second
)

// WAVFmt describes the PCM in a stream.
type WAVFmt struct{ Rate, Channels, Bits uint32 }

// WAVHeader builds a streaming WAV header (unknown length) for 16-bit stereo.
func WAVHeader(rate int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 0xFFFFFFFF)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 2) // channels
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*4))
	binary.LittleEndian.PutUint16(h[32:], 4)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], 0xFFFFFFFF)
	return h
}

// Stream is the audio a renderer pulls over HTTP: the WAV header followed by
// PCM, held in a growing buffer. Renderers (Sonos in particular) probe a URL,
// hang up after ~80 KB and reconnect, so the stream must outlive any single
// connection; each connection reads from its own position.
type Stream struct {
	ID   int
	Fmt  WAVFmt
	Hdr  []byte
	Pace *Pacer // nil = serve as fast as the renderer asks

	// ContentType is the MIME type served; empty means audio/wav.
	ContentType string

	mu   sync.Mutex
	cond *sync.Cond
	data []byte // stream bytes starting at absolute offset base (header first)
	base int64
	hw   int64 // highest absolute offset served to any connection

	metaMu sync.Mutex // ICY metadata marks, see icy.go
	marks  []metaMark

	closed, aborted, accounted, everConnected bool
	conns, connGen                            int
}

// NewStream creates a stream whose first bytes are hdr.
func NewStream(id int, f WAVFmt, hdr []byte) *Stream {
	st := &Stream{ID: id, Fmt: f, Hdr: hdr, data: append([]byte(nil), hdr...)}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *Stream) end() int64 { return st.base + int64(len(st.data)) } // mu held

// pcm converts an absolute stream offset to PCM bytes (without the header).
func (st *Stream) pcm(off int64) int64 {
	if off < int64(len(st.Hdr)) {
		return 0
	}
	return off - int64(len(st.Hdr))
}

// AppendData adds PCM to the stream. It returns false if the stream is closed.
func (st *Stream) AppendData(p []byte) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return false
	}
	st.data = append(st.data, p...)
	st.cond.Broadcast()
	return true
}

// Appended is the total number of bytes added to the stream so far, header included.
func (st *Stream) Appended() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.end()
}

// Served is the highest byte offset any connection has been sent.
func (st *Stream) Served() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hw
}

// HumanBytes formats a byte count for logs.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Finished reports whether the stream has been ended (gracefully or not).
func (st *Stream) Finished() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed
}

// Drained reports whether every byte appended so far has been served.
func (st *Stream) Drained() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hw >= st.end()
}

// ConnState reports the open connection count, a generation number that
// changes with every new connection, and whether any connection was ever made.
func (st *Stream) ConnState() (conns, gen int, ever bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.conns, st.connGen, st.everConnected
}

// Finish ends the stream. A graceful finish lets connections drain what is
// buffered; abort stops them at once. It returns the PCM bytes that were
// buffered but never served (abort only, counted once) so the caller can count
// them as consumed for flow control.
func (st *Stream) Finish(abort bool) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.closed = true
	var unserved int64
	if abort {
		st.aborted = true
		if !st.accounted {
			st.accounted = true
			unserved = st.pcm(st.end()) - st.pcm(st.hw)
		}
	}
	st.cond.Broadcast()
	return unserved
}

// ServeHooks connects Serve to its owner.
type ServeHooks struct {
	Name     string        // for log lines
	Clen     int64         // Content-Length to advertise; 0 = chunked
	Consumed func(n int64) // new PCM bytes served (a high-water mark; called with the stream locked, keep it quick)
	Idle     func(gen int) // the last connection closed; gen identifies the connection generation
	ICY      bool          // answer "Icy-Metadata: 1" with in-band track metadata (not with a fixed Content-Length)
}

// Serve answers one renderer request for this stream.
func (st *Stream) Serve(w http.ResponseWriter, r *http.Request, h ServeHooks) {
	if Verbose {
		log.Printf("[%s] renderer requested %s %s from %s (Range: %q, User-Agent: %q)", h.Name, r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"), r.Header.Get("User-Agent"))
	} else {
		log.Printf("[%s] renderer requested %s %s from %s (Range: %q)", h.Name, r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"))
	}
	hd := w.Header()
	ct := st.ContentType
	if ct == "" {
		ct = "audio/wav"
	}
	hd.Set("Content-Type", ct)
	hd.Set("Accept-Ranges", "none")
	hd.Set("Cache-Control", "no-cache")
	hd["transferMode.dlna.org"] = []string{"Streaming"}
	if h.Clen > 0 {
		hd.Set("Content-Length", strconv.FormatInt(h.Clen, 10))
	}
	var icy *icyWriter
	if h.ICY && h.Clen == 0 && r.Header.Get("Icy-Metadata") == "1" && r.Method != http.MethodHead {
		hd.Set("icy-metaint", strconv.Itoa(ICYInterval))
		hd.Set("icy-name", h.Name)
		icy = &icyWriter{w: w, meta: func(end int64) (string, string) { return st.metaAt(st.pcm(end)) }}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	// A renderer typically probes (reads a little, hangs up) and then
	// reconnects. Early on we replay from the start; later we send the header
	// again and carry on from where the stream got to.
	st.mu.Lock()
	st.conns++
	st.connGen++
	st.everConnected = true
	pos, needHdr := int64(0), false
	if st.hw > replayLimit {
		pos, needHdr = st.hw, true
	}
	st.mu.Unlock()

	var total int64
	began := time.Now()
	why := "renderer closed the connection" // overwritten where we know better
	defer func() {
		note := ""
		if total >= 4<<30-64<<20 {
			note = " (this is the 4 GiB limit of a WAV file's length field)"
		}
		log.Printf("[%s] renderer stream ended after %s (%d bytes) in %s%s: %s", h.Name, HumanBytes(total), total, time.Since(began).Round(time.Second), note, why)
		st.mu.Lock()
		st.conns--
		n, gen := st.conns, st.connGen
		st.mu.Unlock()
		if n == 0 && h.Idle != nil {
			h.Idle(gen)
		}
	}()
	go func() { // wake the wait below when the renderer goes away
		<-r.Context().Done()
		st.mu.Lock()
		st.cond.Broadcast()
		st.mu.Unlock()
	}()

	fl, _ := w.(http.Flusher)
	if needHdr {
		var err error
		if icy != nil {
			err = icy.write(st.Hdr, int64(len(st.Hdr)))
		} else {
			_, err = w.Write(st.Hdr)
		}
		if err != nil {
			why = "write failed: " + err.Error()
			return
		}
	}
	buf := make([]byte, 32<<10)
	for {
		st.mu.Lock()
		for !st.closed && pos >= st.end() && r.Context().Err() == nil {
			st.cond.Wait()
		}
		if st.aborted || r.Context().Err() != nil || pos >= st.end() {
			switch {
			case r.Context().Err() != nil:
				why = "renderer closed the connection (" + r.Context().Err().Error() + ")"
			case st.aborted:
				why = "stream aborted by GoSlim (stop, flush, idle or error)"
			default:
				why = "stream finished, all audio served"
			}
			st.mu.Unlock()
			return
		}
		if pos < st.base {
			pos = st.base
		}
		n := copy(buf, st.data[pos-st.base:])
		st.mu.Unlock()

		if st.Pace != nil {
			if d := st.Pace.Take(n); d > 0 {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					why = "renderer closed the connection while we paced (" + r.Context().Err().Error() + ")"
					return
				}
			}
		}
		var err error
		if icy != nil {
			err = icy.write(buf[:n], pos+int64(n))
		} else {
			_, err = w.Write(buf[:n])
		}
		if err != nil {
			why = "write failed: " + err.Error()
			return
		}
		if fl != nil {
			fl.Flush()
		}
		total += int64(n)
		pos += int64(n)

		st.mu.Lock()
		if pos > st.hw {
			if h.Consumed != nil {
				h.Consumed(st.pcm(pos) - st.pcm(st.hw))
			}
			st.hw = pos
		}
		if served := st.hw - st.base; served > keepServed+(1<<20) {
			cut := served - keepServed
			st.data = st.data[cut:]
			st.base += cut
		}
		st.mu.Unlock()
	}
}

// Pacer is a token bucket that limits how fast a stream is served to the
// renderer. The bucket starts full (an initial burst of cap bytes lets the
// renderer start playing and fill its buffer) and then refills at the
// playback rate, so the renderer can never hold much more than cap bytes
// ahead of what it is playing.
type Pacer struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	cap    float64 // bytes
	tokens float64
	last   time.Time
}

// NewPacer returns a full bucket of capBytes refilling at rate bytes/second.
func NewPacer(rate, capBytes float64) *Pacer {
	return &Pacer{rate: rate, cap: capBytes, tokens: capBytes, last: time.Now()}
}

// Take accounts for n bytes about to be sent and returns how long to wait first.
func (p *Pacer) Take(n int) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.tokens += now.Sub(p.last).Seconds() * p.rate
	if p.tokens > p.cap {
		p.tokens = p.cap
	}
	p.last = now
	p.tokens -= float64(n)
	if p.tokens >= 0 {
		return 0
	}
	return time.Duration(-p.tokens / p.rate * float64(time.Second))
}

// Drain empties the bucket. Used when the renderer pauses: it keeps what it
// has buffered, and after resuming it must not burst a second buffer's worth.
func (p *Pacer) Drain() {
	p.mu.Lock()
	p.tokens = 0
	p.last = time.Now()
	p.mu.Unlock()
}
