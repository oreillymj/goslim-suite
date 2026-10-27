package uclient

import (
	"net"
	"testing"
)

func TestDialFallsBackToDiscovery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()

	calls := 0
	s := &Session{Discover: func() (string, error) { calls++; return ln.Addr().String(), nil }}
	c, addr, err := s.dial(deadAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if addr != ln.Addr().String() || calls != 1 {
		t.Fatalf("addr %s, discover calls %d", addr, calls)
	}
	// the discovered address is remembered, so the next dial does not browse again
	c, _, err = s.dial(deadAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if calls != 1 {
		t.Errorf("browsed again: %d calls", calls)
	}
	// without Discover the configured address is all there is
	if _, _, err = (&Session{}).dial(deadAddr); err == nil {
		t.Error("dial to a closed port succeeded")
	}
}
