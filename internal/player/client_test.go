package player

import (
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
