// Package lms finds a Logitech Media Server / Lyrion Music Server on the
// LAN and shares the result between all the Slimproto players in a process.
//
// Players call Finder.Address before every connection attempt. The first call
// does the UDP discovery; the others wait for it and reuse the answer, so only
// one discovery broadcast is ever in flight.
//
// The address comes from, in order:
//   - a server given on the command line (SetServer with strict=true): trusted,
//     never replaced;
//   - a server from a config file (SetServer with strict=false): used until it
//     stops answering, then replaced by discovery;
//   - a discovery broadcast, which is repeated if the found address stops
//     answering.
package lms

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// DialError marks a failed attempt to connect to LMS, as opposed to a
// connection that was made and later dropped. Players wrap their dial errors
// in it so Finder.Failed can be told about them.
type DialError struct{ Err error }

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// WithPort appends the default Slimproto port (3483) if addr has none.
func WithPort(addr string) string {
	if !strings.Contains(addr, ":") {
		return addr + ":3483"
	}
	return addr
}

// Finder is the shared server lookup. The zero value is ready to use.
type Finder struct {
	mu         sync.Mutex
	configured string // from the command line or the config file; "" = discover
	strict     bool   // configured came from the command line: never fall back
	hintFile   string // config file to suggest saving a discovered address in
	fallback   bool   // the configured address failed, so we are discovering
	found      string // discovered address
	fails      int    // consecutive failed connection attempts to the current address
}

// SetServer sets the configured address (host or host:port). strict means it
// came from the command line and is never replaced by discovery.
func (f *Finder) SetServer(addr string, strict bool) {
	f.mu.Lock()
	f.configured, f.strict = addr, strict
	f.mu.Unlock()
}

// Configured returns the configured address, or "" if there is none.
func (f *Finder) Configured() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configured
}

// SetHintFile names a config file to mention when a discovered address could
// be saved in it.
func (f *Finder) SetHintFile(path string) {
	f.mu.Lock()
	f.hintFile = path
	f.mu.Unlock()
}

// Address returns the host:port to connect to, discovering it if necessary.
func (f *Finder) Address() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock() // held during discovery on purpose: other players wait for the answer
	if f.configured != "" && (f.strict || !f.fallback) {
		return WithPort(f.configured), nil
	}
	if f.found != "" {
		return f.found, nil
	}
	addr, err := Discover(3 * time.Second)
	if err != nil {
		return "", err
	}
	f.found, f.fails = addr, 0
	if f.hintFile != "" {
		log.Printf("LMS found at %s. Add \"server\": %q to %s to skip discovery at startup.", addr, addr, f.hintFile)
	} else {
		log.Printf("LMS found at %s", addr)
	}
	return addr, nil
}

// Failed records a failed connection attempt to addr. After three in a row a
// config-file or discovered address is dropped in favour of discovery.
func (f *Finder) Failed(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails++; f.fails < 3 {
		return
	}
	f.fails = 0
	switch {
	case f.configured != "" && !f.strict && !f.fallback && addr == WithPort(f.configured):
		f.fallback = true
		log.Printf("LMS at %s (from the config file) is not reachable; trying discovery", addr)
	case f.found != "" && addr == f.found:
		f.found = ""
		log.Printf("LMS at %s is not reachable; searching again", addr)
	}
}

// Connected records a successful connection.
func (f *Finder) Connected() {
	f.mu.Lock()
	f.fails = 0
	f.mu.Unlock()
}

// ForConfig returns the LMS address to write into a generated config file: the
// command-line server if given, otherwise whatever a discovery broadcast finds.
func ForConfig(flagServer string) string {
	if flagServer != "" {
		return WithPort(flagServer)
	}
	addr, err := Discover(3 * time.Second)
	if err != nil {
		fmt.Printf("\n(LMS not found by broadcast: %v; add a \"server\" entry to the config file by hand)\n", err)
		return ""
	}
	return addr
}

// Discover broadcasts a Slimproto discovery request and returns the host:port
// of the first server that answers.
func Discover(timeout time.Duration) (string, error) {
	locals := localIPv4s() // one socket per adapter: a plain broadcast only leaves through one of them
	if len(locals) == 0 {
		locals = []net.IP{nil}
	}
	found := make(chan string, len(locals))
	deadline := time.Now().Add(timeout)
	for _, ip := range locals {
		go func(ip net.IP) {
			var laddr *net.UDPAddr
			if ip != nil {
				laddr = &net.UDPAddr{IP: ip}
			}
			pc, err := net.ListenUDP("udp4", laddr)
			if err != nil {
				return
			}
			defer pc.Close()
			buf := make([]byte, 1024)
			for i := 0; i < 3 && time.Now().Before(deadline); i++ { // three tries in case one datagram is lost
				pc.WriteTo([]byte("e"), &net.UDPAddr{IP: net.IPv4bcast, Port: 3483})
				pc.SetReadDeadline(time.Now().Add(timeout / 3))
				if _, from, err := pc.ReadFrom(buf); err == nil {
					select {
					case found <- net.JoinHostPort(from.(*net.UDPAddr).IP.String(), "3483"):
					default:
					}
					return
				}
			}
		}(ip)
	}
	select {
	case addr := <-found:
		return addr, nil
	case <-time.After(timeout + 500*time.Millisecond):
		return "", errors.New("no reply to the LMS discovery broadcast")
	}
}

func localIPv4s() []net.IP {
	var out []net.IP
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&(net.FlagBroadcast|net.FlagMulticast) == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLinkLocalUnicast() {
					out = append(out, ip4)
				}
			}
		}
	}
	return out
}
