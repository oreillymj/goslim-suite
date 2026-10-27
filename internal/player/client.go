// Package player implements a Slimproto (Squeezebox) player: it talks to LMS,
// receives the audio streams LMS sends, decodes them and queues the PCM for an
// output, which is either the local sound device (PlayLocal) or an Output such
// as a UPnP renderer.
package player

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goslim/internal/decode"
	"goslim/internal/lms"
)

// WebConfig says where to look up track information (LMS's web interface).
type WebConfig struct {
	Port       int // 0 turns track logging off
	User, Pass string
}

// Options configure a Client.
type Options struct {
	Name       string
	MAC        net.HardwareAddr
	OutRate    int             // sample rate of the PCM this player produces
	MaxRate    int             // highest sample rate offered to LMS; 0 = OutRate
	Decoder    *decode.Decoder // nil = native formats only
	Finder     *lms.Finder     // shared server lookup; nil = a private one
	ReplayGain bool            // apply the gain LMS asks for
	Web        WebConfig

	Passthrough bool // don't decode: hand LMS's original stream to the output (experimental)
	NoResample  bool // keep each track's own sample rate instead of resampling to OutRate
	Verbose     bool // log what each track is
}

type track struct {
	buf       *pcmBuf
	conn      net.Conn
	started   bool
	played    int64 // output PCM bytes consumed
	autostart byte
	threshold int
	lSent     bool
	gain      float64      // ReplayGain multiplier from LMS (1 = none)
	pcmFmt    [4]byte      // sample size, rate, channels, endianness chars from strm (PCM streams without a header)
	rate      atomic.Int32 // sample rate of the PCM in buf, set by the first decoded audio
	raw       bool         // buf holds the undecoded stream (passthrough)
	format    byte         // format letter
}

// Client is one Slimproto player.
type Client struct {
	opts    Options
	name    string
	mac     net.HardwareAddr
	outRate int
	finder  *lms.Finder
	start   time.Time
	quit    chan struct{} // closed by Close
	wake    chan struct{} // pulsed when queued audio or the playing state may have changed

	out Output // non-nil when audio goes to something other than the local device

	conn net.Conn
	wmu  sync.Mutex

	mu      sync.Mutex
	queue   []*track
	playing bool
	volume  float64

	bytesRecv atomic.Uint64
}

// New creates a player. Call SetOutput (if any) before Loop.
func New(o Options) *Client {
	if o.Finder == nil {
		o.Finder = &lms.Finder{}
	}
	if o.Decoder == nil {
		o.Decoder = &decode.Decoder{OutRate: o.OutRate}
	}
	return &Client{
		opts: o, name: o.Name, mac: o.MAC, outRate: o.OutRate, finder: o.Finder,
		start: time.Now(), quit: make(chan struct{}), wake: make(chan struct{}, 1), volume: 1,
	}
}

// SetOutput directs the player's audio to out instead of the local device.
func (c *Client) SetOutput(out Output) { c.out = out }

func (c *Client) Name() string          { return c.name }
func (c *Client) MAC() net.HardwareAddr { return c.mac }
func (c *Client) OutRate() int          { return c.outRate }
func (c *Client) Done() <-chan struct{} { return c.quit }

// Options returns the options the player was created with.
func (c *Client) Options() Options { return c.opts }

// Wake is pulsed whenever more audio may be available from Pull, or the
// playing state changed. Outputs wait on it instead of polling.
func (c *Client) Wake() <-chan struct{} { return c.wake }

func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// atHead reports whether t is the track at the front of the queue.
func (c *Client) atHead(t *track) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue) > 0 && c.queue[0] == t
}

// Volume is the volume LMS last asked for, as a linear gain (1.0 = full scale).
func (c *Client) Volume() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.volume
}

// Notify reports a playback event to LMS (STMs, STMu, STMp, ...) without blocking.
func (c *Client) Notify(event string) { go c.stat(event, 0) }

// PauseFromOutput pauses the player because the output was paused or stopped
// on its own side, and tells LMS. It reports whether it was playing.
func (c *Client) PauseFromOutput() bool {
	c.mu.Lock()
	was := c.playing
	c.playing = false
	c.mu.Unlock()
	if was {
		c.Notify("STMp")
	}
	c.signal()
	return was
}

// ResumeFromOutput resumes the player because the output resumed on its own
// side, and tells LMS.
func (c *Client) ResumeFromOutput() {
	c.mu.Lock()
	c.playing = len(c.queue) > 0
	c.mu.Unlock()
	c.Notify("STMr")
	c.signal()
}

// Close stops the player: it leaves LMS, drops its queue and closes its output.
func (c *Client) Close() {
	select {
	case <-c.quit:
		return
	default:
		close(c.quit)
	}
	c.wmu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.wmu.Unlock()
	c.flush()
	c.signal()
	if c.out != nil {
		c.out.Close()
	}
}

// ---------------------------------------------------------------- protocol out

func (c *Client) send(op string, data []byte) error {
	hdr := make([]byte, 8)
	copy(hdr, op)
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(data)))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.conn == nil {
		return errors.New("not connected")
	}
	_, err := c.conn.Write(append(hdr, data...))
	return err
}

func (c *Client) stat(event string, serverTS uint32) {
	c.mu.Lock()
	buffered := 0
	elapsed := 0.0
	for _, t := range c.queue {
		buffered += t.buf.len()
	}
	if len(c.queue) > 0 {
		t := c.queue[0]
		played := t.played
		if c.out != nil {
			if played -= c.out.Inflight(); played < 0 {
				played = 0
			}
		}
		rate := int(t.rate.Load())
		if rate == 0 {
			rate = c.outRate
		}
		elapsed = float64(played) / float64(rate*4)
	}
	c.mu.Unlock()
	if c.out != nil {
		if e, ok := c.out.Elapsed(); ok { // e.g. a passthrough renderer: wall-clock time
			elapsed = e.Seconds()
		}
	}

	b := make([]byte, 53)
	copy(b[0:4], event)
	be := binary.BigEndian
	be.PutUint32(b[7:], maxBuffered)
	be.PutUint32(b[11:], uint32(buffered))
	be.PutUint64(b[15:], c.bytesRecv.Load())
	be.PutUint16(b[23:], 0xffff)
	be.PutUint32(b[25:], uint32(time.Since(c.start).Milliseconds()))
	be.PutUint32(b[37:], uint32(elapsed))
	be.PutUint32(b[43:], uint32(elapsed*1000))
	be.PutUint32(b[47:], serverTS)
	c.send("STAT", b)
}

func (c *Client) sendHELO() error {
	b := make([]byte, 36)
	b[0] = 12 // device id (squeezelite)
	copy(b[2:8], c.mac)
	copy(b[34:36], "en")
	maxRate := c.opts.MaxRate
	if maxRate <= 0 {
		maxRate = c.outRate
	}
	caps := fmt.Sprintf("Model=squeezelite,ModelName=GoSlim,Firmware=0.1,MaxSampleRate=%d,%s", maxRate, c.opts.Decoder.Caps())
	return c.send("HELO", append(b, caps...))
}

func (c *Client) flush() {
	c.mu.Lock()
	q := c.queue
	c.queue = nil
	c.playing = false
	c.mu.Unlock()
	for _, t := range q {
		t.buf.close()
		t.conn.Close()
	}
	if c.out != nil {
		c.out.Flushed()
	}
	c.signal()
}

// ---------------------------------------------------------------- protocol in

func (c *Client) handle(op string, d []byte) {
	switch op {
	case "strm":
		c.handleStrm(d)
	case "audg":
		if len(d) >= 18 && d[8] != 0 {
			l := float64(binary.BigEndian.Uint32(d[10:14])) / 65536
			r := float64(binary.BigEndian.Uint32(d[14:18])) / 65536
			v := math.Min(1, (l+r)/2)
			c.mu.Lock()
			c.volume = v
			c.mu.Unlock()
			if c.out != nil {
				c.out.SetVolume(v)
			}
		} else if len(d) >= 18 {
			c.mu.Lock()
			c.volume = 1
			c.mu.Unlock()
			if c.out != nil {
				c.out.SetVolume(1)
			}
		}
	case "setd":
		if len(d) >= 1 && d[0] == 0 {
			if len(d) == 1 {
				c.send("SETD", append(append([]byte{0}, c.name...), 0))
			} else {
				c.name = strings.TrimRight(string(d[1:]), "\x00")
				log.Printf("player name set to %q", c.name)
				if c.out != nil {
					c.out.SetName(c.name)
				}
			}
		}
	case "serv":
		log.Printf("server requested switch (ignored)")
	default: // aude, vers, cont, ledc, visu, grfe ... ignored
	}
}

func (c *Client) handleStrm(d []byte) {
	if len(d) < 24 {
		return
	}
	switch d[0] {
	case 's':
		c.startStream(d)
	case 'p':
		c.mu.Lock()
		c.playing = false
		c.mu.Unlock()
		if c.out != nil {
			c.out.Pause()
		}
		c.stat("STMp", 0)
	case 'u':
		c.mu.Lock()
		c.playing = len(c.queue) > 0
		c.mu.Unlock()
		c.signal()
		if c.out != nil {
			c.out.Resume()
		}
		c.stat("STMr", 0)
	case 'q', 'f':
		c.flush()
		c.stat("STMf", 0)
	case 't':
		c.stat("STMt", binary.BigEndian.Uint32(d[14:18]))
	}
}

func (c *Client) startStream(d []byte) {
	autostart, format := d[1], d[2]
	if !c.opts.Decoder.Supported(format) {
		log.Printf("unsupported format %q", format)
		c.stat("STMn", 0)
		return
	}
	ip := net.IP(d[20:24])
	host := ip.String()
	if ip.Equal(net.IPv4zero) {
		host, _, _ = net.SplitHostPort(c.conn.RemoteAddr().String())
	}
	port := binary.BigEndian.Uint16(d[18:20])
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 5*time.Second)
	if err != nil {
		log.Printf("stream connect: %v", err)
		c.stat("STMn", 0)
		return
	}
	req := append([]byte(nil), d[24:]...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		c.stat("STMn", 0)
		return
	}
	gain := 1.0
	if g := binary.BigEndian.Uint32(d[14:18]); g != 0 && c.opts.ReplayGain {
		gain = float64(g) / 65536
		log.Printf("[%s] replay gain %+.2f dB", c.name, 20*math.Log10(gain))
	}
	t := &track{buf: newPCMBuf(), conn: conn, autostart: autostart, threshold: int(d[7]) * 1024, gain: gain, raw: c.opts.Passthrough, format: format}
	copy(t.pcmFmt[:], d[3:7])
	if t.raw && c.opts.ReplayGain && gain != 1 {
		log.Printf("[%s] passthrough: ReplayGain cannot be applied to an undecoded stream", c.name)
	}

	c.mu.Lock()
	if len(c.queue) == 0 {
		c.playing = autostart == '1' || autostart == '3'
	}
	c.queue = append(c.queue, t)
	c.mu.Unlock()

	c.signal()
	c.stat("STMc", 0)
	c.stat("STMe", 0)
	if t.raw {
		go c.relayTrack(t, format)
	} else {
		go c.decodeTrack(t, format)
	}
}

// ---------------------------------------------------------------- decoding

type countReader struct {
	r io.Reader
	n *atomic.Uint64
}

func (cr *countReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n.Add(uint64(n))
	return n, err
}

// openBody reads the HTTP response headers of a track's stream. On failure it
// reports STMn to LMS and returns false.
func (c *Client) openBody(t *track) (*bufio.Reader, bool) {
	br := bufio.NewReaderSize(t.conn, 64<<10)
	status, err := br.ReadString('\n')
	ok := err == nil && strings.Contains(status, " 200")
	for ok {
		line, err := br.ReadString('\n')
		if err != nil {
			ok = false
		} else if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !ok {
		log.Printf("bad HTTP response: %q (%v)", status, err)
		t.buf.finish()
		c.stat("STMn", 0)
		c.signal()
		return nil, false
	}
	c.stat("STMh", 0)
	return br, true
}

// trackDone finishes a track's buffer and tells LMS the decoder has it all.
func (c *Client) trackDone(t *track) {
	t.buf.finish()
	c.signal()
	if !t.buf.isClosed() {
		if !t.lSent && t.autostart == '0' {
			t.lSent = true
			c.stat("STMl", 0)
		}
		c.stat("STMd", 0)
	}
}

func (c *Client) decodeTrack(t *track, format byte) {
	defer t.conn.Close()
	br, ok := c.openBody(t)
	if !ok {
		return
	}
	src := &countReader{r: br, n: &c.bytesRecv}
	s := &sink{c: c, t: t}
	var onInfo func(decode.Info)
	if c.opts.Verbose {
		onInfo = func(i decode.Info) { c.logTrack(i, t.gain) }
	}
	err := c.opts.Decoder.DecodeInfo(format, src, t.pcmFmt, s.write, onInfo)
	if err != nil && !errors.Is(err, errClosed) {
		log.Printf("decode: %v", err)
	}
	c.trackDone(t)
}

// logTrack says, in verbose mode, what a track is and what is done to it.
func (c *Client) logTrack(i decode.Info, gain float64) {
	what := "PCM at its own sample rate"
	switch {
	case c.opts.Passthrough:
		what = "passed through undecoded"
	case i.Rate == 0:
		what = fmt.Sprintf("PCM at %g kHz", float64(c.outRate)/1000)
	case !c.opts.NoResample && i.Rate != c.outRate:
		what = fmt.Sprintf("decoded, resampled %g -> %g kHz", float64(i.Rate)/1000, float64(c.outRate)/1000)
	case !c.opts.NoResample:
		what = fmt.Sprintf("decoded to PCM at %g kHz", float64(i.Rate)/1000)
	}
	extra := ""
	if gain != 1 {
		extra = fmt.Sprintf(", ReplayGain %+.2f dB", 20*math.Log10(gain))
	}
	log.Printf("[%s] track: %s -> %s%s", c.name, i, what, extra)
}

// rawPrefetch is how much of a passthrough track is read ahead of its turn.
const rawPrefetch = 1 << 20

// relayTrack is decodeTrack for passthrough mode: the body is queued as it is.
func (c *Client) relayTrack(t *track, format byte) {
	defer t.conn.Close()
	br, ok := c.openBody(t)
	if !ok {
		return
	}
	head, _ := br.Peek(16 << 10)
	if format == decode.FormatPCM && !(len(head) >= 4 && (string(head[:4]) == "RIFF" || string(head[:4]) == "FORM")) {
		hdr, ok := decode.RawPCMHeader(t.pcmFmt)
		if !ok {
			log.Printf("[%s] passthrough: header-less PCM that WAV cannot carry; skipping the track", c.name)
			t.buf.finish()
			c.stat("STMn", 0)
			c.signal()
			return
		}
		if t.buf.write(hdr) != nil {
			return
		}
	}
	if c.opts.Verbose {
		if i, ok := decode.Probe(format, head, t.pcmFmt); ok {
			c.logTrack(i, 1)
		}
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			// Don't read far ahead of the track that is playing: LMS would
			// then send the whole playlist and run ahead of the audio.
			for t.buf.len() >= rawPrefetch && !c.atHead(t) {
				select {
				case <-c.quit:
					return
				case <-time.After(200 * time.Millisecond):
				}
				if t.buf.isClosed() {
					return
				}
			}
			if t.buf.write(buf[:n]) != nil {
				return
			}
			c.bytesRecv.Add(uint64(n))
			c.signal()
		}
		if err != nil {
			break
		}
	}
	c.trackDone(t)
}

// sink applies ReplayGain, resamples to the output rate (unless that is turned
// off) and queues the PCM.
type sink struct {
	c  *Client
	t  *track
	rs *decode.Resampler
}

func (s *sink) write(in []int16, rate int) error {
	if g := s.t.gain; g != 1 {
		for i, v := range in {
			in[i] = decode.Clip16(float64(v) * g)
		}
	}
	out, outRate := in, rate
	if !s.c.opts.NoResample && rate != s.c.outRate {
		if s.rs == nil {
			s.rs = decode.NewResampler(rate, s.c.outRate)
		}
		out, outRate = s.rs.Process(in), s.c.outRate
	}
	b := make([]byte, len(out)*2)
	for i, v := range out {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	s.t.rate.CompareAndSwap(0, int32(outRate))
	if err := s.t.buf.write(b); err != nil {
		return err
	}
	s.c.signal()
	if !s.t.lSent && s.t.autostart == '0' && s.t.buf.len() >= s.t.threshold {
		s.t.lSent = true
		s.c.stat("STMl", 0)
	}
	return nil
}

// ---------------------------------------------------------------- connection

func (c *Client) run(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return &lms.DialError{Err: err}
	}
	c.wmu.Lock()
	select {
	case <-c.quit:
		c.wmu.Unlock()
		conn.Close()
		return errors.New("client shut down")
	default:
	}
	c.conn = conn
	c.wmu.Unlock()
	defer conn.Close()
	defer c.flush()

	if err := c.sendHELO(); err != nil {
		return err
	}
	log.Printf("[%s] connected to LMS at %s", c.name, addr)

	done := make(chan struct{})
	defer close(done)
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				c.stat("STMt", 0)
			case <-done:
				return
			}
		}
	}()

	var hdr [2]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		if n < 4 {
			continue
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		c.handle(string(body[:4]), body[4:])
	}
}

// Loop keeps the player connected to LMS until Close is called. The server
// address comes from the lookup shared by all players in the process.
func (c *Client) Loop() {
	short := 0 // consecutive connections that LMS closed within seconds
	for {
		select {
		case <-c.quit:
			return
		default:
		}
		addr, err := c.finder.Address()
		if err != nil {
			log.Printf("[%s] cannot find LMS: %v (retrying; or use -server host)", c.name, err)
			select {
			case <-c.quit:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		began := time.Now()
		err = c.run(addr)
		var de *lms.DialError
		if errors.As(err, &de) {
			c.finder.Failed(addr)
		} else {
			c.finder.Connected()
		}
		select {
		case <-c.quit:
			return
		default:
		}
		log.Printf("[%s] disconnected: %v; retrying in 5s", c.name, err)
		if errors.Is(err, io.EOF) && time.Since(began) < 30*time.Second {
			if short++; short == 3 {
				log.Printf("[%s] LMS keeps closing this connection within seconds. The usual cause is another player using the same MAC (%s): "+
					"another copy of this program or squeezelite. Stop it, or give this player its own MAC.", c.name, c.mac)
			}
		} else {
			short = 0
		}
		select {
		case <-c.quit:
			return
		case <-time.After(5 * time.Second):
		}
	}
}
