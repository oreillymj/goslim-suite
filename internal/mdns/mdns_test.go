package mdns

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestMessageRoundTrip(t *testing.T) {
	in := &message{response: true, answers: []record{
		{name: "_x._tcp.local.", rtype: typePTR, ttl: 120, target: "a b._x._tcp.local."},
		{name: "a b._x._tcp.local.", rtype: typeSRV, ttl: 120, port: 40123, target: "host.local."},
		{name: "a b._x._tcp.local.", rtype: typeTXT, ttl: 120, txt: []string{"ver=1"}},
		{name: "host.local.", rtype: typeA, ttl: 120, ip: net.IPv4(192, 168, 1, 7)},
	}}
	b, err := in.pack()
	if err != nil {
		t.Fatal(err)
	}
	out, err := parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !out.response || len(out.answers) != 4 {
		t.Fatalf("got %+v", out)
	}
	if out.answers[1].port != 40123 || out.answers[1].target != "host.local." || out.answers[0].target != "a b._x._tcp.local." {
		t.Errorf("wrong records: %+v", out.answers)
	}
	if !out.answers[3].ip.Equal(net.IPv4(192, 168, 1, 7)) || out.answers[2].txt[0] != "ver=1" {
		t.Errorf("wrong records: %+v", out.answers)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, {1, 2, 3}, append(make([]byte, 12), 0xc0, 0x0c), {0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 5, 'a'}} {
		parse(b) // must not panic
	}
	loop := append(make([]byte, 12), 0xc0, 12)
	loop[5] = 1
	if _, err := parse(loop); err == nil {
		t.Error("pointer loop accepted")
	}
}

// TestAdvertiseAndBrowse needs a multicast capable interface; it is skipped where there is none.
func TestAdvertiseAndBrowse(t *testing.T) {
	if len(multicastInterfaces()) == 0 {
		t.Skip("no multicast interface")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Advertise(ctx, Service{Instance: "GoSlim test 41234", Port: 41234, Text: []string{"ver=1"}}); err != nil {
		t.Skip(err)
	}
	es, err := Browse(context.Background(), 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) == 0 {
		t.Skip("multicast does not loop back here")
	}
	for _, e := range es {
		if e.Port == 41234 && e.Instance == "GoSlim test 41234" {
			return
		}
	}
	t.Errorf("service not among %+v", es)
}
