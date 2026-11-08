package player

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"
)

// Transitions between tracks, as LMS asks for them in the strm 's' header
// (Slim::Player::Squeezebox::stream_s) and as Squeezelite carries them out
// (output.c: _checkfade). LMS decides everything - whether a transition type is
// set at all, the smart-crossfade rule (no transition between neighbouring tracks
// of one album), the sample-rate restriction, the sleep-timer override, "track
// shorter than twice the duration" (duration/3) and the 10 s maximum - and puts
// the result in the header of a track:
//
//	d[9]  = seconds, d[10] = '0' none, '1' crossfade, '2' fade in, '3' fade out,
//	        '4' fade in and out, '5' crossfade (immediate; LMS does not send it)
//
// Like Squeezelite, GoSlim reads the type from the track's own header:
//
//	'1','5'  at the START of this track its first N seconds are mixed with the
//	         last N seconds of the track before it (so it starts N seconds early)
//	'2'      at the START of this track it fades in over N seconds
//	'3'      at the END of this track it fades out over its last N seconds
//	'4'      at the END of this track it fades out over N/2 seconds, and the
//	         track that follows fades in over the same N/2 seconds
const (
	maxTransitionSecs = 30 // LMS never sends more than 10; this only protects memory
	pcmFrame          = 4  // bytes per frame: 16-bit stereo
)

// transitionNames uses LMS's own constant names (Slim::Player::Squeezebox), so a GoSlim log can be
// compared line for line with LMS's server.log.
var transitionNames = map[byte]string{
	'0': "TRANSITION_NONE", '1': "TRANSITION_CROSSFADE", '2': "TRANSITION_FADEIN", '3': "TRANSITION_FADEOUT",
	'4': "TRANSITION_FADEINOUT", '5': "TRANSITION_CROSSFADE_IMMEDIATE",
}

// transitionName returns "TRANSITION_CROSSFADE (1)" for the header's ASCII type byte.
func transitionName(kind byte) string {
	if n, ok := transitionNames[kind]; ok {
		return fmt.Sprintf("%s (%c)", n, kind)
	}
	return fmt.Sprintf("unknown (0x%02x)", kind)
}

// transition is what a strm header asks for when its track starts.
type transition struct {
	kind byte
	secs int
}

func parseTransition(d []byte) transition {
	t := transition{kind: d[10], secs: int(d[9])}
	if t.secs > maxTransitionSecs {
		t.secs = maxTransitionSecs
	}
	if t.kind < '1' || t.kind > '5' || t.secs == 0 {
		return transition{kind: '0'}
	}
	return t
}

func (t transition) crossfade() bool { return t.kind == '1' || t.kind == '5' }
func (t transition) fadeIn() bool    { return t.kind == '2' }
func (t transition) fadeOut() bool   { return t.kind == '3' || t.kind == '4' }

// bytes is the transition's length in PCM bytes at the given sample rate. Fade in and out splits
// the time between the two halves, as Squeezelite does.
func (t transition) bytes(rate int) int {
	n := t.secs * rate * pcmFrame
	if t.kind == '4' {
		n /= 2
	}
	return n &^ (pcmFrame - 1)
}

// mixState is a crossfade in progress: the end of from is being mixed with the start of to.
type mixState struct {
	from, to    *track
	total, done int // bytes
}

// noteTransition records, for a track that has just been requested, the transition LMS asked for
// and remembers the longest one so far. mu is held.
func (c *Client) noteTransition(t *track, d []byte) {
	t.trans = parseTransition(d)
	if c.opts.NoTransitions || t.raw {
		t.trans = transition{kind: '0'}
		return
	}
	if t.trans.crossfade() && t.trans.secs > c.armSecs {
		c.armSecs = t.trans.secs // how much of every track's end is worth holding back
	}
}

// EnableTailHold makes Pull keep the last seconds of a fully decoded track back until LMS has said
// what follows it. An output that runs ahead of the speakers (a UPnP renderer buffers many seconds)
// needs it, because otherwise the end of a short track is already on its way to the renderer when the
// next track - and the crossfade it asks for - arrives. An output that pulls in real time does not.
func (c *Client) EnableTailHold() {
	c.mu.Lock()
	c.holdTail = true
	c.mu.Unlock()
}

// ReleaseHold lets Pull hand out a held tail. The output calls it when nothing is going to follow
// (the renderer is running out of audio), so the end of the last track is never cut off.
func (c *Client) ReleaseHold() {
	c.mu.Lock()
	c.holdRelease = true
	c.mu.Unlock()
	c.signal()
}

// pullTail works out how much of the head track t may be handed out now, and whether it is time to mix.
// It returns the byte limit for a plain read of t (len(p)-bound is applied by the caller), the track to
// mix with (when a crossfade starts now) or held=true when t's tail is being kept back. mu is held.
func (c *Client) pullTail(t *track) (limit int, mixWith *track, held bool) {
	limit = -1 // no limit
	if t.raw || !t.buf.complete() {
		return
	}
	rate := int(t.rate.Load())
	if rate == 0 {
		return
	}
	rem := t.buf.len() &^ (pcmFrame - 1)
	if len(c.queue) > 1 {
		nxt := c.queue[1]
		tr := nxt.trans
		switch {
		case tr.crossfade():
			nrate := int(nxt.rate.Load())
			if nxt.raw || (nrate != 0 && nrate != rate) {
				if !nxt.xfadeLogged {
					nxt.xfadeLogged = true
					log.Printf("[%s] crossfade skipped: the tracks differ in sample rate (%d / %d Hz) or are not decoded", c.name, rate, nrate)
				}
				return
			}
			if nrate == 0 {
				return 0, nil, false // wait for the next track's first audio so both rates are known
			}
			l := tr.bytes(rate)
			if rem > l {
				return rem - l, nil, false // play on normally until the overlap begins
			}
			return 0, nxt, false
		}
		return
	}
	if c.holdTail && c.armSecs > 0 && !c.holdRelease {
		reserve := c.armSecs * rate * pcmFrame
		if rem <= reserve {
			return 0, nil, true
		}
		return rem - reserve, nil, false
	}
	return
}

// startMix begins a crossfade from the head track into the next one.
func (c *Client) startMix(from, to *track) {
	total := from.buf.len() &^ (pcmFrame - 1)
	c.mix = &mixState{from: from, to: to, total: total}
	log.Printf("[%s] ==> crossfade: mixing the last %.1f s of this track with the start of the next (LMS asked for %s over %d s)",
		c.name, float64(total)/float64(int(from.rate.Load())*pcmFrame), transitionName(to.trans.kind), to.trans.secs)
}

// mixStep mixes up to len(p) bytes of the running crossfade into p. It returns the bytes written.
// ended reports that the crossfade is complete and the head track was dropped from the queue.
// mu is held.
func (c *Client) mixStep(p []byte, r *PullResult) (n int, ended bool) {
	m := c.mix
	room := len(p) &^ (pcmFrame - 1)
	if left := m.total - m.done; room > left {
		room = left
	}
	if room > m.to.buf.len() {
		room = m.to.buf.len() &^ (pcmFrame - 1)
	}
	if room == 0 {
		if m.to.buf.finished() { // the new track was shorter than the overlap: plain playback of what is left
			c.mix = nil
		}
		return 0, false
	}
	a := make([]byte, room)
	b := make([]byte, room)
	na, nb := m.from.buf.read(a), m.to.buf.read(b)
	if na < room || nb < room { // cannot happen (the tail is fully decoded), but never emit garbage
		room = (na &^ (pcmFrame - 1))
		if nb < na {
			room = nb &^ (pcmFrame - 1)
		}
	}
	mix(p[:room], a[:room], b[:room], m.done, m.total, c.curve)
	if !m.to.started {
		m.to.started = true
		r.Started = true
		r.Starts = append(r.Starts, r.N)
		go c.stat("STMs", 0)
	}
	m.from.played += int64(room)
	m.to.played += int64(room)
	m.done += room
	if m.done >= m.total {
		c.queue = c.queue[1:]
		r.Ended++
		c.mix = nil
		return room, true
	}
	return room, false
}

// A curve maps the position x (0..1) within a fade to the gain of the track that is fading IN; the
// track fading out uses curve(1-x). Squeezelite's curve is a straight line.
type curve func(x float64) float64

// logRangeDB is how far below full level the logarithmic curve starts.
const logRangeDB = 40

var (
	curveLinear curve = func(x float64) float64 { return x } // Squeezelite: gain = position (to_gain(f) = f*65536)
	// curvePower keeps the combined power of two unrelated tracks constant: no dip in the middle.
	curvePower curve = func(x float64) float64 { return math.Sin(x * math.Pi / 2) }
	// curveLog is a ramp that is linear in decibels (from -logRangeDB to 0 dB), shifted so that it still
	// starts at silence. It follows loudness perception, but both tracks are 20 dB down in the middle of a
	// crossfade, so there is a clear dip; it suits fades in and out better than crossfades.
	curveLog curve = func(x float64) float64 {
		floor := math.Pow(10, -logRangeDB/20.0)
		return (math.Pow(10, -logRangeDB*(1-x)/20.0) - floor) / (1 - floor)
	}
)

// CurveNames lists the values -crossfadecurve accepts.
const CurveNames = "linear, power, log"

// ParseCurve turns a -crossfadecurve value into a curve; "" means linear.
func ParseCurve(name string) (func(float64) float64, error) {
	switch name {
	case "", "linear":
		return curveLinear, nil
	case "power":
		return curvePower, nil
	case "log":
		return curveLog, nil
	}
	return nil, fmt.Errorf("unknown crossfade curve %q (use %s)", name, CurveNames)
}

// mix writes the crossfade of a (fading out) and b (fading in) to dst. start and total are byte
// positions within the whole crossfade, so a crossfade spread over several calls stays continuous.
func mix(dst, a, b []byte, start, total int, f curve) {
	frames := total / pcmFrame
	f0 := start / pcmFrame
	for i := 0; i+pcmFrame <= len(dst); i += pcmFrame {
		x := float64(f0+i/pcmFrame) / float64(frames)
		gin, gout := f(x), f(1-x)
		for ch := 0; ch < 2; ch++ {
			o := i + ch*2
			u := float64(int16(binary.LittleEndian.Uint16(a[o:])))
			v := float64(int16(binary.LittleEndian.Uint16(b[o:])))
			binary.LittleEndian.PutUint16(dst[o:], uint16(clip(u*gout+v*gin)))
		}
	}
}

// fadeInPCM scales p, which holds the audio starting pos bytes into a track, up from silence over the
// first total bytes.
func fadeInPCM(p []byte, pos, total int, f curve) {
	if pos >= total {
		return
	}
	for i := 0; i+pcmFrame <= len(p) && pos+i < total; i += pcmFrame {
		scale(p[i:i+pcmFrame], f(float64(pos+i)/float64(total)))
	}
}

// fadeOutPCM scales p down to silence over the last total bytes of a track. rem is how many bytes of
// the track were left before p was read, so p[i] is rem-i bytes from the end.
func fadeOutPCM(p []byte, rem, total int, f curve) {
	for i := 0; i+pcmFrame <= len(p); i += pcmFrame {
		if left := rem - i; left < total {
			scale(p[i:i+pcmFrame], f(float64(left)/float64(total)))
		}
	}
}

func scale(frame []byte, g float64) {
	for ch := 0; ch < 2; ch++ {
		v := float64(int16(binary.LittleEndian.Uint16(frame[ch*2:])))
		binary.LittleEndian.PutUint16(frame[ch*2:], uint16(clip(v*g)))
	}
}

func clip(v float64) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	}
	return int16(v)
}

// transitionText describes a header's transition for the log.
func transitionText(t transition) string {
	return fmt.Sprintf("%s over %d s", transitionName(t.kind), t.secs)
}

// HoldPoll is how often an output that is holding back a tail checks whether to give it up.
const HoldPoll = 100 * time.Millisecond

// shape applies the fades LMS asked for to p, the audio just read from t: fade in at the start of t, the
// fade up that follows a fade in-out, and fade out at t's own end. mu is held.
func (c *Client) shape(t *track, p []byte) {
	rate := int(t.rate.Load())
	if rate == 0 {
		return
	}
	if t.trans.fadeIn() {
		fadeInPCM(p, int(t.played), t.trans.bytes(rate), c.curve)
	}
	if c.fadeUp > 0 {
		fadeInPCM(p, int(t.played), c.fadeUp, c.curve)
		if int(t.played)+len(p) >= c.fadeUp {
			c.fadeUp = 0
		}
	}
	if t.trans.fadeOut() && t.buf.complete() {
		fadeOutPCM(p, len(p)+t.buf.len(), t.trans.bytes(rate), c.curve)
	}
}

// trackEnded is called when the head track has been played out. After a fade in-out the next track
// fades in over the length of the fade-out.
func (c *Client) trackEnded(t *track) {
	if t.trans.kind == '4' && !t.raw {
		if rate := int(t.rate.Load()); rate != 0 {
			c.fadeUp = t.trans.bytes(rate)
		}
	}
}
