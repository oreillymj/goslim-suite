package player

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func TestStallDetection(t *testing.T) {

	c := New(Options{StallTimeout: time.Minute, OnStall: func(string) {}})
	c.mu.Lock()
	c.playing = true
	c.mu.Unlock()
	if !c.StreamLost() {
		t.Fatal("StreamLost: was not playing")
	}
	if _, ok := c.stalled(time.Now().Add(30 * time.Second)); ok {
		t.Error("stalled too early")
	}
	if why, ok := c.stalled(time.Now().Add(2 * time.Minute)); !ok || why == "" {
		t.Error("not stalled after the timeout")
	}
	// LMS reacts (here: a pause command): no longer stalled.
	c.handleStrm(append([]byte{'p'}, make([]byte, 30)...))
	if _, ok := c.stalled(time.Now().Add(time.Hour)); ok {
		t.Error("stalled although LMS paused the player")
	}
	// A status request is not a reaction.
	c.mu.Lock()
	c.playing = true
	c.mu.Unlock()
	c.StreamLost()
	c.handleStrm(append([]byte{'t'}, make([]byte, 30)...))
	if _, ok := c.stalled(time.Now().Add(time.Hour)); !ok {
		t.Error("a status request cleared the stall")
	}
	// Off by default.
	if _, ok := New(Options{}).stalled(time.Now().Add(time.Hour)); ok {
		t.Error("stall check ran with no timeout")
	}

}

func TestLogStrmHeader(t *testing.T) {
	var b bytes.Buffer
	log.SetOutput(&b)
	defer log.SetOutput(io.Discard)
	c := New(Options{Name: "t"})
	d := make([]byte, 30)
	copy(d, []byte{'s', '1', 'f', '?', '?', '?', '?', 255, 0, 10, '1', 0x83, 5, 0})
	binary.BigEndian.PutUint32(d[14:], 0x10000)
	binary.BigEndian.PutUint16(d[18:], 9000)
	copy(d[20:], []byte{192, 168, 2, 20})
	c.logStrmHeader(d)
	for _, want := range []string{"transition TRANSITION_CROSSFADE (1) over 10 s", "flags 0x83", "output threshold 5", "replay gain 0x00010000", "192.168.2.20:9000", "raw 73 31 66"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
}
