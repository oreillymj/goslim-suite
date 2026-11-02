package upnpout

import (
	"net"
	"testing"
	"time"

	"goslim/internal/player"
	"goslim/internal/upnp"
)

// A renderer that stops answering and then shows up under another address is followed there.
func TestLocateFollowsNewAddress(t *testing.T) {
	defer func(a, b time.Duration) { locateEvery, locateAfter = a, b }(locateEvery, locateAfter)
	locateEvery, locateAfter = 20*time.Millisecond, 0

	c := player.New(player.Options{})
	defer c.Close()
	oldR := &upnp.Renderer{IP: net.ParseIP("192.0.2.1"), MAC: "aa:bb:cc:dd:ee:ff"}
	newR := &upnp.Renderer{IP: net.ParseIP("192.0.2.2"), MAC: "aa:bb:cc:dd:ee:ff"}
	moved := make(chan [2]net.IP, 1)
	o := &Out{c: c, name: "t", up: upnp.NewClient(oldR, "t"),
		rediscover: func() *upnp.Renderer { return newR },
		onMoved:    func(a, b net.IP) { moved <- [2]net.IP{a, b} },
		kick:       make(chan struct{}, 1)}
	o.failSince.Store(time.Now().Add(-time.Minute).UnixNano())
	go o.locate()
	select {
	case m := <-moved:
		if !m[0].Equal(oldR.IP) || !m[1].Equal(newR.IP) {
			t.Errorf("moved %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the new address was not picked up")
	}
	if got := o.up.Renderer().IP; !got.Equal(newR.IP) {
		t.Errorf("client still uses %v", got)
	}
}
