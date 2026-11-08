package player

import (
	"encoding/binary"
	"testing"
)

const tRate = 100 // Hz: keeps the test audio tiny (one second = 400 bytes)

// pcmTrack is a fully decoded track of secs seconds, every sample val, with the transition LMS asked for.
func pcmTrack(rate, secs int, val int16, kind byte, tsecs int) *track {
	t := &track{buf: newPCMBuf(), started: false, gain: 1}
	t.rate.Store(int32(rate))
	b := make([]byte, secs*rate*pcmFrame)
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(val))
	}
	t.buf.write(b)
	t.buf.finish()
	t.trans = parseTransition([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, byte(tsecs), kind})
	return t
}

func newTestClient(tracks ...*track) *Client {
	c := New(Options{OutRate: tRate})
	c.playing = true
	c.queue = tracks
	return c
}

// drain pulls everything the client will give (in small odd-sized chunks) and returns the samples of
// the left channel plus the sum of the Pull results.
func drain(c *Client) (left []int16, res PullResult) {
	buf := make([]byte, 52*pcmFrame/4*4)
	for i := 0; i < 10000; i++ {
		r := c.Pull(buf)
		for j := 0; j+pcmFrame <= r.N; j += pcmFrame {
			left = append(left, int16(binary.LittleEndian.Uint16(buf[j:])))
		}
		res.Ended += r.Ended
		res.Emptied = res.Emptied || r.Emptied
		res.Held = res.Held || r.Held
		if r.Started {
			res.Started = true
		}
		if r.N == 0 {
			break
		}
	}
	return
}

func TestParseTransition(t *testing.T) {
	for _, tc := range []struct {
		kind byte
		secs int
		want byte
	}{{'0', 10, '0'}, {'1', 0, '0'}, {'1', 10, '1'}, {'5', 3, '5'}, {'9', 3, '0'}, {'3', 99, '3'}} {
		got := parseTransition([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, byte(tc.secs), tc.kind})
		if got.kind != tc.want {
			t.Errorf("kind %q secs %d: got %q, want %q", tc.kind, tc.secs, got.kind, tc.want)
		}
		if got.secs > maxTransitionSecs {
			t.Errorf("secs %d not capped", got.secs)
		}
	}
}

func TestCrossfadeMixesTheEndWithTheStart(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '0', 0)
	b := pcmTrack(tRate, 30, 3000, '1', 10)
	c := newTestClient(a, b)
	out, res := drain(c)
	if want := (30 + 30 - 10) * tRate; len(out) != want {
		t.Fatalf("got %d frames, want %d (the overlap must shorten the playlist by the crossfade time)", len(out), want)
	}
	if out[0] != 1000 || out[20*tRate-1] != 1000 {
		t.Errorf("track A should play unchanged before the overlap: %d, %d", out[0], out[20*tRate-1])
	}
	mid := out[25*tRate]
	if mid < 1900 || mid > 2100 {
		t.Errorf("middle of the crossfade = %d, want about 2000", mid)
	}
	for i := 20 * tRate; i < 30*tRate; i++ { // monotonic, between the two levels
		if out[i] < out[i-1] || out[i] < 1000 || out[i] > 3000 {
			t.Fatalf("frame %d = %d: crossfade is not a smooth ramp", i, out[i])
		}
	}
	if out[30*tRate] != 3000 || out[len(out)-1] != 3000 {
		t.Errorf("track B should play unchanged after the overlap: %d, %d", out[30*tRate], out[len(out)-1])
	}
	if res.Ended != 2 || !res.Emptied {
		t.Errorf("ended %d emptied %v, want 2 true", res.Ended, res.Emptied)
	}
}

func TestNoTransitionJoinsTracks(t *testing.T) {
	c := newTestClient(pcmTrack(tRate, 30, 1000, '0', 0), pcmTrack(tRate, 30, 3000, '0', 10))
	out, _ := drain(c)
	if len(out) != 60*tRate || out[30*tRate-1] != 1000 || out[30*tRate] != 3000 {
		t.Errorf("tracks should join gaplessly: %d frames, %d | %d", len(out), out[30*tRate-1], out[30*tRate])
	}
}

func TestCrossfadeSkippedWhenRatesDiffer(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '0', 0)
	b := pcmTrack(tRate*2, 30, 3000, '1', 10)
	c := newTestClient(a, b)
	buf := make([]byte, 4096)
	total := 0
	for i := 0; i < 100; i++ {
		r := c.Pull(buf)
		total += r.N
		if r.N == 0 {
			break
		}
	}
	if total != 30*tRate*pcmFrame+30*tRate*2*pcmFrame {
		t.Errorf("tracks at different rates must play whole, one after the other; got %d bytes", total)
	}
}

// Squeezelite fades a track out at its own end, as its own header says; fade in-out splits the time and
// the next track fades in straight afterwards.
func TestFadeInOutUsesTheTracksOwnHeader(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '4', 6) // 3 s down at the end of A, then 3 s up on whatever follows
	b := pcmTrack(tRate, 30, 3000, '0', 0)
	out, _ := drain(newTestClient(a, b))
	if len(out) != 60*tRate {
		t.Fatalf("fades must not overlap the tracks: %d frames", len(out))
	}
	if out[27*tRate-1] != 1000 {
		t.Errorf("A before its fade-out: %d", out[27*tRate-1])
	}
	if mid := out[28*tRate+tRate/2]; mid < 450 || mid > 550 {
		t.Errorf("A halfway through its fade-out = %d, want about 500", mid)
	}
	if last := out[30*tRate-1]; last > 20 {
		t.Errorf("end of A should be silent, got %d", last)
	}
	if first := out[30*tRate]; first > 40 {
		t.Errorf("start of B should be silent, got %d", first)
	}
	if mid := out[31*tRate+tRate/2]; mid < 1450 || mid > 1550 {
		t.Errorf("B halfway through its fade-in = %d, want about 1500", mid)
	}
	if out[33*tRate] != 3000 {
		t.Errorf("B at full level after the fade-in: %d", out[33*tRate])
	}
}

func TestFadeOutOnly(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '3', 5)
	b := pcmTrack(tRate, 30, 3000, '0', 0)
	out, _ := drain(newTestClient(a, b))
	if out[25*tRate-1] != 1000 || out[30*tRate-1] > 20 || out[30*tRate] != 3000 {
		t.Errorf("fade out over the last 5 s, next track unfaded: %d %d %d", out[25*tRate-1], out[30*tRate-1], out[30*tRate])
	}
}

func TestNextTracksHeaderDoesNotFadeThePreviousTrack(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '0', 0)
	b := pcmTrack(tRate, 30, 3000, '3', 5)
	out, _ := drain(newTestClient(a, b))
	if out[30*tRate-1] != 1000 {
		t.Errorf("A's end must not be faded by B's header: %d", out[30*tRate-1])
	}
	if out[60*tRate-1] > 20 {
		t.Errorf("B's own end should fade out: %d", out[60*tRate-1])
	}
}

func TestFadeInOnly(t *testing.T) {
	b := pcmTrack(tRate, 20, 3000, '2', 4)
	out, _ := drain(newTestClient(b))
	if out[0] != 0 || out[2*tRate] < 1400 || out[2*tRate] > 1600 || out[4*tRate] != 3000 {
		t.Errorf("fade in over 4 s: %d, %d, %d", out[0], out[2*tRate], out[4*tRate])
	}
}

// The renderer runs ahead of the speakers, so the end of a fully decoded track is held back until LMS
// says what follows; if nothing does, the output releases it.
func TestTailHeldUntilNextTrackOrRelease(t *testing.T) {
	a := pcmTrack(tRate, 30, 1000, '1', 10) // LMS used a crossfade for this one: later tracks are likely to as well
	c := newTestClient(a)
	c.armSecs, c.holdTail = 10, true
	out, res := drain(c)
	if len(out) != 20*tRate || !res.Held || res.Ended != 0 {
		t.Fatalf("pulled %d frames (held=%v, ended=%d); want the last 10 s kept back", len(out), res.Held, res.Ended)
	}

	// Case 1: nothing follows. The output releases the tail.
	c.ReleaseHold()
	rest, res := drain(c)
	if len(rest) != 10*tRate || !res.Emptied {
		t.Errorf("after release got %d frames, emptied=%v", len(rest), res.Emptied)
	}

	// Case 2: a next track arrives while the tail is held: it is crossfaded into.
	a = pcmTrack(tRate, 30, 1000, '1', 10)
	c = newTestClient(a)
	c.armSecs, c.holdTail = 10, true
	pre, _ := drain(c)
	c.mu.Lock()
	c.queue = append(c.queue, pcmTrack(tRate, 30, 3000, '1', 10))
	c.mu.Unlock()
	post, res := drain(c)
	if !res.Held {
		t.Error("the new last track's own tail should now be the one held back")
	}
	c.ReleaseHold()
	last, _ := drain(c)
	if got, want := len(pre)+len(post)+len(last), 50*tRate; got != want {
		t.Errorf("held tail then crossfade: %d frames, want %d", got, want)
	}
}

func TestHoldIgnoredWithoutEnableOrArming(t *testing.T) {
	c := newTestClient(pcmTrack(tRate, 30, 1000, '0', 0))
	c.armSecs, c.holdTail = 10, false
	if out, _ := drain(c); len(out) != 30*tRate {
		t.Errorf("hold must be off for outputs that pull in real time: %d frames", len(out))
	}
	c = newTestClient(pcmTrack(tRate, 30, 1000, '0', 0))
	c.holdTail = true // LMS never asked for a transition: nothing to hold for
	if out, _ := drain(c); len(out) != 30*tRate {
		t.Errorf("hold must not apply when LMS never asked for a transition: %d frames", len(out))
	}
}

func TestNoTransitionsOption(t *testing.T) {
	c := New(Options{OutRate: tRate, NoTransitions: true})
	tr := &track{}
	c.noteTransition(tr, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 10, '1'})
	if tr.trans.kind != '0' || c.armSecs != 0 {
		t.Errorf("-crossfade=false must ignore the request: %+v armSecs %d", tr.trans, c.armSecs)
	}
}

func TestCurves(t *testing.T) {
	for _, name := range []string{"linear", "power", "log"} {
		f, err := ParseCurve(name)
		if err != nil {
			t.Fatal(err)
		}
		if f(0) > 1e-9 || f(1) < 1-1e-9 {
			t.Errorf("%s: must run from silence (%g) to full level (%g)", name, f(0), f(1))
		}
		for x := 0.01; x <= 1; x += 0.01 {
			if f(x) < f(x-0.01) {
				t.Fatalf("%s: not monotonic at %g", name, x)
			}
		}
	}
	if _, err := ParseCurve("sine"); err == nil {
		t.Error("an unknown curve must be rejected")
	}
	p, _ := ParseCurve("power")
	if sum := p(0.3)*p(0.3) + p(0.7)*p(0.7); sum < 0.999 || sum > 1.001 {
		t.Errorf("power curve: combined power = %g, want 1", sum)
	}
	l, _ := ParseCurve("log")
	if g := l(0.5); g < 0.07 || g > 0.12 {
		t.Errorf("log curve at the middle = %g, want about -20 dB (0.09)", g)
	}
}

func TestCrossfadeCurveChangesTheMix(t *testing.T) {
	for name, want := range map[string][2]int16{"linear": {1950, 2050}, "power": {2780, 2880}, "log": {300, 450}} {
		c := New(Options{OutRate: tRate, CrossfadeCurve: name})
		c.playing = true
		c.queue = []*track{pcmTrack(tRate, 30, 1000, '0', 0), pcmTrack(tRate, 30, 3000, '1', 10)}
		out, _ := drain(c)
		if mid := out[25*tRate]; mid < want[0] || mid > want[1] {
			t.Errorf("%s: middle of the crossfade = %d, want %d..%d", name, mid, want[0], want[1])
		}
	}
}
