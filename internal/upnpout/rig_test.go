package upnpout

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"goslim/internal/decode"
	"goslim/internal/lms"
	"goslim/internal/player"
	"goslim/internal/upnp"
)

func testWAV(rate int, secs float64) []byte {
	n := int(float64(rate) * secs)
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	binary.Write(&b, le, uint32(36+n*4))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, le, uint32(16))
	binary.Write(&b, le, uint16(1))
	binary.Write(&b, le, uint16(2))
	binary.Write(&b, le, uint32(rate))
	binary.Write(&b, le, uint32(rate*4))
	binary.Write(&b, le, uint16(4))
	binary.Write(&b, le, uint16(16))
	b.WriteString("data")
	binary.Write(&b, le, uint32(n*4))
	for i := 0; i < n; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/float64(rate)))
		binary.Write(&b, le, v)
		binary.Write(&b, le, v)
	}
	return b.Bytes()
}

// testTrack is one file the fake LMS sends.
type testTrack struct {
	format byte // m, f or p
	body   []byte
}

type rigOptions struct {
	tracks      []testTrack
	noResample  bool
	passthrough bool
	autoResume  bool
	rotate      int64
	dropAfter   int64 // the renderer hangs up on its first stream after this many bytes
	idleOnly    bool  // connect the player but send nothing
	webPort     int   // LMS web port for volume/track lookups
	volume      bool  // the fake LMS sends a volume change after HELO
	noFetch     bool  // the renderer accepts the stream URL but never requests it (a firewall in the way)
	netFail     func(string)
	replayGain  uint32 // 16.16 fixed point gain LMS puts in the strm command (0 = none)
	icy         bool   // ICY metadata on, and the fake renderer asks for it
}

// rig is a fake LMS, a fake UPnP renderer and the player under test.
type rig struct {
	metaint string // icy-metaint the renderer was given (guarded by mu)
	t       *testing.T
	o       rigOptions
	c       *player.Client
	out     *Out

	mu       sync.Mutex
	events   []string // Slimproto STAT events the player sent
	actions  []string // UPnP actions the renderer received
	uris     []string
	metas    []string
	bodies   map[int][]byte
	ctypes   map[int]string
	uriTimes map[int]time.Time
	endTimes map[int]time.Time
	active   int
	sentNext int
}

func (g *rig) has(event string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.events {
		if e == event {
			return true
		}
	}
	return false
}

func (g *rig) count(action string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, a := range g.actions {
		if a == action {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newRig(t *testing.T, o rigOptions) *rig {
	g := &rig{t: t, o: o, bodies: map[int][]byte{}, ctypes: map[int]string{}, uriTimes: map[int]time.Time{}, endTimes: map[int]time.Time{}}
	upnp.ReconnectGrace = 400 * time.Millisecond

	// LMS's stream server: GET /<n> returns track n.
	hln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hln.Close() })
	go func() {
		for {
			c, err := hln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req := make([]byte, 4096)
				n, _ := c.Read(req)
				var i int
				if _, err := fmtSscan(string(req[:n]), &i); err != nil || i >= len(o.tracks) {
					return
				}
				c.Write(append([]byte("HTTP/1.0 200 OK\r\n\r\n"), o.tracks[i].body...))
			}()
		}
	}()
	httpPort := hln.Addr().(*net.TCPAddr).Port

	// The fake LMS: send track 0 after HELO, and the next track after each STMd.
	lln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lln.Close() })
	go func() {
		c, err := lln.Accept()
		if err != nil {
			return
		}
		frame := func(op string, d []byte) []byte {
			return append(append(binary.BigEndian.AppendUint16(nil, uint16(4+len(d))), op...), d...)
		}
		sendNext := func() {
			g.mu.Lock()
			i := g.sentNext
			g.sentNext++
			g.mu.Unlock()
			if i >= len(o.tracks) {
				return
			}
			hdr := append([]byte{'s', '1', o.tracks[i].format, '?', '?', '?', '?', 0, 0, 0, '0', 0, 0, 0}, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(hdr[14:18], o.replayGain)
			hdr = binary.BigEndian.AppendUint16(hdr, uint16(httpPort))
			hdr = append(hdr, 0, 0, 0, 0)
			c.Write(frame("strm", append(hdr, []byte("GET /"+itoa(i)+" HTTP/1.0\r\n\r\n")...)))
		}
		for {
			var h [8]byte
			if _, err := io.ReadFull(c, h[:]); err != nil {
				return
			}
			d := make([]byte, binary.BigEndian.Uint32(h[4:]))
			io.ReadFull(c, d)
			switch string(h[:4]) {
			case "HELO":
				if o.volume {
					gain := binary.BigEndian.AppendUint32(nil, 0x4000) // 0.25
					c.Write(frame("audg", append(append(make([]byte, 8), 1, 0), append(gain, gain...)...)))
				}
				if !o.idleOnly {
					sendNext()
				}
			case "STAT":
				ev := string(d[:4])
				g.mu.Lock()
				g.events = append(g.events, ev)
				g.mu.Unlock()
				if ev == "STMd" {
					sendNext()
				}
			}
		}
	}()

	// The fake renderer.
	uriRe := regexp.MustCompile(`<CurrentURI>([^<]*)</CurrentURI>`)
	metaRe := regexp.MustCompile(`<CurrentURIMetaData>([^<]*)</CurrentURIMetaData>`)
	rend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		soap := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
		action := soap[strings.Index(soap, "#")+1:]
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.actions = append(g.actions, action)
		state := "STOPPED"
		if g.active > 0 {
			state = "PLAYING"
		}
		g.mu.Unlock()
		resp := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`
		switch action {
		case "SetAVTransportURI":
			uri := html.UnescapeString(uriRe.FindStringSubmatch(string(body))[1])
			g.mu.Lock()
			idx := len(g.uris)
			g.uris = append(g.uris, uri)
			g.metas = append(g.metas, html.UnescapeString(metaRe.FindStringSubmatch(string(body))[1]))
			g.uriTimes[idx] = time.Now()
			g.active++ // it will fetch the stream: report PLAYING from now
			g.mu.Unlock()
			if !o.noFetch {
				go g.fetch(uri, idx)
			}
		case "GetTransportInfo":
			resp += `<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><CurrentTransportState>` + state + `</CurrentTransportState></u:GetTransportInfoResponse>`
		}
		w.Write([]byte(resp + `</s:Body></s:Envelope>`))
	}))
	t.Cleanup(rend.Close)
	r := &upnp.Renderer{
		Name: "Fake", IP: net.ParseIP("127.0.0.1"),
		AVTransport:      &upnp.Service{Type: "urn:schemas-upnp-org:service:AVTransport:1", ControlURL: rend.URL + "/av"},
		RenderingControl: &upnp.Service{Type: "urn:schemas-upnp-org:service:RenderingControl:1", ControlURL: rend.URL + "/rc"},
	}

	finder := &lms.Finder{}
	finder.SetServer(lln.Addr().String(), true)
	g.c = player.New(player.Options{
		Name: "T", MAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, OutRate: 48000,
		Decoder: &decode.Decoder{OutRate: 48000}, Finder: finder,
		NoResample: o.noResample, Passthrough: o.passthrough, Web: player.WebConfig{Port: o.webPort}, ReplayGain: true,
	})
	g.out = New(g.c, r, Options{Name: "T", Idle: 30 * time.Second, VolMax: 100, AutoResume: o.autoResume, RotateBytes: o.rotate, OnNetFail: o.netFail, ICY: o.icy})
	g.c.SetOutput(g.out)
	sln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.out.Serve(sln)
	g.out.Run()
	go g.c.Loop()
	t.Cleanup(g.c.Close)
	return g
}

// fetch plays the part of the renderer pulling the stream it was given.
func (g *rig) fetch(uri string, idx int) {
	defer func() {
		g.mu.Lock()
		g.active--
		g.endTimes[idx] = time.Now()
		g.mu.Unlock()
	}()
	req, _ := http.NewRequest("GET", uri, nil)
	if g.o.icy {
		req.Header.Set("Icy-Metadata", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	limit := int64(1 << 60)
	if idx == 0 && g.o.dropAfter > 0 {
		limit = g.o.dropAfter
	}
	g.mu.Lock()
	g.ctypes[idx] = resp.Header.Get("Content-Type")
	g.metaint = resp.Header.Get("icy-metaint")
	g.mu.Unlock()
	buf := make([]byte, 32<<10)
	for total := int64(0); total < limit; {
		want := int64(len(buf))
		if limit-total < want {
			want = limit - total
		}
		n, err := resp.Body.Read(buf[:want])
		if n > 0 { // record as it arrives: a live stream doesn't end
			g.mu.Lock()
			g.bodies[idx] = append(g.bodies[idx], buf[:n]...)
			g.mu.Unlock()
			total += int64(n)
		}
		if err != nil {
			return
		}
	}
}

func (g *rig) body(i int) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]byte(nil), g.bodies[i]...)
}

func itoa(i int) string { return string(rune('0' + i)) }

var fmtSscanf = fmt.Sscanf

func fmtSscan(req string, i *int) (int, error) {
	// "GET /<n> HTTP/1.0"
	var n int
	_, err := fmtSscanf(req, "GET /%d ", &n)
	*i = n
	return n, err
}
