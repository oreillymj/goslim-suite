package upnp

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verbose enables extra diagnostics in the log.
var Verbose bool

type Service struct {
	Type       string
	ControlURL string // absolute
}

type Renderer struct {
	Name, Manufacturer, Model, UDN string
	IP                             net.IP
	MAC                            string // aa:bb:cc:dd:ee:ff, "" if unresolved
	Location                       string
	AVTransport                    *Service
	RenderingControl               *Service
	ZoneTopology                   *Service // Sonos zone group / stereo pair info (may be nil)
}

type xmlService struct {
	Type       string `xml:"serviceType"`
	ControlURL string `xml:"controlURL"`
}

type xmlDevice struct {
	FriendlyName string       `xml:"friendlyName"`
	Manufacturer string       `xml:"manufacturer"`
	ModelName    string       `xml:"modelName"`
	UDN          string       `xml:"UDN"`
	Services     []xmlService `xml:"serviceList>service"`
	Devices      []xmlDevice  `xml:"deviceList>device"`
}

type xmlRoot struct {
	URLBase string    `xml:"URLBase"`
	Device  xmlDevice `xml:"device"`
}

func NormMAC(s string) string {
	var hex []byte
	for _, c := range strings.ToLower(s) {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			hex = append(hex, byte(c))
		}
	}
	if len(hex) != 12 {
		return ""
	}
	var parts []string
	for i := 0; i < 12; i += 2 {
		parts = append(parts, string(hex[i:i+2]))
	}
	return strings.Join(parts, ":")
}

func localIPv4s() []net.IP {
	var out []net.IP
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
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

// ssdpSearch sends M-SEARCH from every local IPv4 address and returns the
// unique LOCATION URLs that answer.
func ssdpSearch(locals []net.IP, timeout time.Duration) []string {
	const tmpl = "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: %s\r\n\r\n"
	targets := []string{
		"urn:schemas-upnp-org:device:MediaRenderer:1",
		"urn:schemas-upnp-org:service:AVTransport:1",
	}
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, ip := range locals {
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
			if err != nil {
				if Verbose {
					log.Printf("ssdp: listen on %s: %v", ip, err)
				}
				return
			}
			defer pc.Close()
			deadline := time.Now().Add(timeout)
			pc.SetReadDeadline(deadline)
			for round := 0; round < 2; round++ {
				for _, st := range targets {
					pc.WriteTo([]byte(strings.Replace(tmpl, "%s", st, 1)), dst)
				}
				time.Sleep(300 * time.Millisecond)
			}
			buf := make([]byte, 4096)
			for {
				n, _, err := pc.ReadFrom(buf)
				if err != nil {
					// Windows reports ICMP "port unreachable" as a read error; keep going.
					if time.Now().After(deadline) {
						return
					}
					continue
				}
				resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
				if err != nil {
					continue
				}
				if loc := resp.Header.Get("Location"); loc != "" {
					mu.Lock()
					seen[loc] = true
					mu.Unlock()
				}
			}
		}(ip)
	}
	wg.Wait()
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	return out
}

// describe fetches the device description. It returns nil, nil when the
// device has no AVTransport service (i.e. it can't play audio).
func describe(loc string) (*Renderer, error) {
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Get(loc)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var root xmlRoot
	if err := xml.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, err
	}
	base, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	r := &Renderer{Location: loc, IP: net.ParseIP(base.Hostname())}
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil {
			base = b
		}
	}

	var walk func(d *xmlDevice) bool
	walk = func(d *xmlDevice) bool {
		var av, rc *Service
		for _, s := range d.Services {
			ref, err := base.Parse(s.ControlURL)
			if err != nil {
				continue
			}
			sv := &Service{Type: s.Type, ControlURL: ref.String()}
			switch {
			case strings.Contains(s.Type, ":service:AVTransport:"):
				av = sv
			case strings.Contains(s.Type, ":service:RenderingControl:"):
				rc = sv
			}
		}
		if av != nil {
			r.Name, r.Manufacturer, r.Model, r.UDN = d.FriendlyName, d.Manufacturer, d.ModelName, d.UDN
			r.AVTransport, r.RenderingControl = av, rc
			return true
		}
		for i := range d.Devices {
			if walk(&d.Devices[i]) {
				return true
			}
		}
		return false
	}
	if !walk(&root.Device) {
		return nil, nil
	}
	var find func(d *xmlDevice) *Service
	find = func(d *xmlDevice) *Service {
		for _, s := range d.Services {
			if strings.Contains(s.Type, ":service:ZoneGroupTopology:") {
				if ref, err := base.Parse(s.ControlURL); err == nil {
					return &Service{Type: s.Type, ControlURL: ref.String()}
				}
			}
		}
		for i := range d.Devices {
			if sv := find(&d.Devices[i]); sv != nil {
				return sv
			}
		}
		return nil
	}
	r.ZoneTopology = find(&root.Device)
	if r.IP != nil {
		// The HTTP request above has just touched this host, so ARP is warm.
		r.MAC = NormMAC(lookupMAC(r.IP))
	}
	return r, nil
}

func DiscoverRenderers(bind net.IP, timeout time.Duration) []*Renderer {
	locals := localIPv4s()
	if bind != nil {
		locals = []net.IP{bind}
	}
	if Verbose {
		log.Printf("discovery from %v, timeout %s", locals, timeout)
	}
	locs := ssdpSearch(locals, timeout)

	var mu sync.Mutex
	var wg sync.WaitGroup
	byUDN := map[string]*Renderer{}
	for _, loc := range locs {
		wg.Add(1)
		go func(loc string) {
			defer wg.Done()
			r, err := describe(loc)
			if err != nil {
				if Verbose {
					log.Printf("describe %s: %v", loc, err)
				}
				return
			}
			if r == nil {
				return
			}
			mu.Lock()
			byUDN[r.UDN+r.Location] = r
			mu.Unlock()
		}(loc)
	}
	wg.Wait()

	// The same UDN can answer on several interfaces/URLs; keep one per UDN.
	uniq := map[string]*Renderer{}
	for _, r := range byUDN {
		if _, ok := uniq[r.UDN]; !ok {
			uniq[r.UDN] = r
		}
	}
	out := make([]*Renderer, 0, len(uniq))
	for _, r := range uniq {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
