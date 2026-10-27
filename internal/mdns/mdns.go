// Package mdns advertises and finds a service with multicast DNS (DNS-SD), so
// that GoSlim's proxy port does not have to be fixed. It is a deliberately small
// implementation: IPv4 only, one service type, answers always carry all records.
package mdns

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// ServiceType is what GoSlim's proxy mode advertises.
const ServiceType = "_goslimproxy._tcp"

var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Service describes what to advertise.
type Service struct {
	Instance string   // e.g. "GoSlim myhost 3434"
	Port     int      // TCP port
	Text     []string // TXT strings, e.g. "ver=1"
}

// Entry is one service found by Browse.
type Entry struct {
	Instance string
	IP       net.IP // the address the answer came from, which is reachable from here
	Port     int
}

func (e Entry) Addr() string { return net.JoinHostPort(e.IP.String(), fmt.Sprint(e.Port)) }

// multicastInterfaces lists the interfaces that are up, multicast capable and
// have an IPv4 address.
func multicastInterfaces() []*net.Interface {
	all, _ := net.Interfaces()
	var out []*net.Interface
	for i := range all {
		ifi := &all[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if len(ipv4Addrs(ifi)) > 0 {
			out = append(out, ifi)
		}
	}
	return out
}

func ipv4Addrs(ifi *net.Interface) []net.IP {
	addrs, _ := ifi.Addrs()
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() {
				out = append(out, ip)
			}
		}
	}
	return out
}

func localIPs() []net.IP {
	var out []net.IP
	for _, ifi := range multicastInterfaces() {
		for _, ip := range ipv4Addrs(ifi) {
			if !ip.IsLoopback() {
				out = append(out, ip)
			}
		}
	}
	return out
}

// sender writes multicast packets out of every interface in turn.
type sender struct {
	mu   sync.Mutex
	pc   *ipv4.PacketConn
	ifis []*net.Interface
}

func (s *sender) multicast(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ifis) == 0 {
		s.pc.WriteTo(b, nil, group)
		return
	}
	for _, ifi := range s.ifis {
		if s.pc.SetMulticastInterface(ifi) == nil {
			s.pc.WriteTo(b, nil, group)
		}
	}
}

func (s *sender) unicast(b []byte, to net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pc.WriteTo(b, nil, to)
}

func hostLabel() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	var b strings.Builder
	for _, r := range h {
		if r < 128 && (r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "goslim"
	}
	return b.String()
}

// Advertise answers queries for svc until ctx is cancelled, then says goodbye.
// It returns once the responder is running, or with an error if it cannot start.
func Advertise(ctx context.Context, svc Service) error {
	ifis := multicastInterfaces()
	var first *net.Interface
	if len(ifis) > 0 {
		first = ifis[0]
	}
	// ListenMulticastUDP sets SO_REUSEADDR, so this works alongside avahi or Bonjour.
	conn, err := net.ListenMulticastUDP("udp4", first, group)
	if err != nil {
		return fmt.Errorf("mDNS: %w", err)
	}
	pc := ipv4.NewPacketConn(conn)
	for _, ifi := range ifis {
		pc.JoinGroup(ifi, group) // already joined on the first one: errors are fine
	}
	pc.SetMulticastLoopback(true) // so a client on this machine hears us
	pc.SetMulticastTTL(255)
	snd := &sender{pc: pc, ifis: ifis}

	serviceName := ServiceType + ".local."
	instance := svc.Instance + "." + serviceName
	host := hostLabel() + ".local."

	records := func(ttl uint32) []record {
		rs := []record{
			{name: serviceName, rtype: typePTR, ttl: ttl, target: instance},
			{name: instance, rtype: typeSRV, ttl: ttl, port: svc.Port, target: host},
			{name: instance, rtype: typeTXT, ttl: ttl, txt: svc.Text},
		}
		for _, ip := range localIPs() {
			rs = append(rs, record{name: host, rtype: typeA, ttl: ttl, ip: ip})
		}
		return rs
	}
	pack := func(ttl uint32) []byte {
		b, _ := (&message{response: true, answers: records(ttl)}).pack()
		return b
	}

	go func() { // answer queries
		buf := make([]byte, 9000)
		for {
			n, _, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			m, err := parse(buf[:n])
			if err != nil || m.response {
				continue
			}
			for _, q := range m.questions {
				name := strings.ToLower(q.name)
				ours := (name == serviceName && (q.qtype == typePTR || q.qtype == typeANY)) ||
					(name == strings.ToLower(instance) && (q.qtype == typeSRV || q.qtype == typeTXT || q.qtype == typeANY)) ||
					(name == strings.ToLower(host) && (q.qtype == typeA || q.qtype == typeANY))
				if !ours {
					continue
				}
				if u, ok := src.(*net.UDPAddr); ok && u.Port != 5353 {
					snd.unicast(pack(120), src) // a simple querier: answer it directly
				} else {
					snd.multicast(pack(120))
				}
				break
			}
		}
	}()
	go func() { // announce now and every minute; say goodbye at the end
		t := time.NewTimer(0)
		defer t.Stop()
		for n := 0; ; n++ {
			select {
			case <-ctx.Done():
				snd.multicast(pack(0))
				conn.Close()
				return
			case <-t.C:
				snd.multicast(pack(120))
				if n < 2 {
					t.Reset(time.Second)
				} else {
					t.Reset(time.Minute)
				}
			}
		}
	}()
	log.Printf("mDNS: advertising %q (%s) on port %d", svc.Instance, ServiceType, svc.Port)
	return nil
}

// Browse looks for services of ServiceType for up to wait, returning as soon as
// something has been found and a moment has passed for others to answer.
func Browse(ctx context.Context, wait time.Duration) ([]Entry, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("mDNS: %w", err)
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	pc.SetMulticastLoopback(true)
	pc.SetMulticastTTL(255)
	snd := &sender{pc: pc, ifis: multicastInterfaces()}

	serviceName := ServiceType + ".local."
	q, _ := (&message{questions: []question{{serviceName, typePTR}}}).pack()
	snd.multicast(q)

	deadline := time.Now().Add(wait)
	nextQuery := time.Now().Add(700 * time.Millisecond)
	var settle time.Time
	ptr := map[string]bool{}
	srvPort := map[string]int{}
	var found []Entry
	seen := map[string]bool{}
	buf := make([]byte, 9000)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if !settle.IsZero() && time.Now().After(settle) {
			break
		}
		if time.Now().After(nextQuery) {
			snd.multicast(q)
			nextQuery = time.Now().Add(time.Second)
		}
		conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		m, err := parse(buf[:n])
		if err != nil || !m.response {
			continue
		}
		for _, r := range m.answers {
			if r.ttl == 0 {
				continue
			}
			switch {
			case r.rtype == typePTR && strings.EqualFold(r.name, serviceName):
				ptr[r.target] = true
			case r.rtype == typeSRV:
				srvPort[r.name] = r.port
			}
		}
		for inst := range ptr {
			port, ok := srvPort[inst]
			if !ok || port == 0 {
				continue
			}
			e := Entry{Instance: strings.TrimSuffix(inst, "."+serviceName), IP: src.IP, Port: port}
			if key := e.Instance + "|" + e.Addr(); !seen[key] {
				seen[key] = true
				found = append(found, e)
				if settle.IsZero() {
					settle = time.Now().Add(400 * time.Millisecond)
				}
			}
		}
	}
	return found, ctx.Err()
}
