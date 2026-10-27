package upnpout

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aacpcm "github.com/tphakala/go-aac/pcm"
	"goslim/internal/decode"
	"goslim/internal/lms"
	"goslim/internal/player"
	"goslim/internal/upnp"
)

func rateOf(stream []byte) int { return int(binary.LittleEndian.Uint32(stream[24:28])) }

func TestStreamsToRenderer(t *testing.T) {
	g := newRig(t, rigOptions{tracks: []testTrack{{'p', testWAV(44100, 1.0)}}})
	want := 44 + 48000*4 // header plus one second of 48 kHz stereo (resampled from 44.1)
	waitFor(t, "the renderer to fetch the stream", 15*time.Second, func() bool { return len(g.body(0)) >= want-4*200 })
	if n := len(g.body(0)); n > want+4*200 || rateOf(g.body(0)) != 48000 {
		t.Fatalf("renderer fetched %d bytes at %d Hz, want about %d at 48000", n, rateOf(g.body(0)), want)
	}
	g.c.Close() // must leave the speaker stopped before returning
	if !strings.HasPrefix(strings.Join(g.actions, ","), "SetAVTransportURI,Play") || g.count("Stop") == 0 {
		t.Errorf("renderer actions: %v", g.actions)
	}
}

// With resampling off each track keeps its own rate, so a change of rate needs a
// new stream, started only after the renderer has played out the old one.
func TestNativeRatesHandOver(t *testing.T) {
	g := newRig(t, rigOptions{noResample: true, tracks: []testTrack{{'p', testWAV(44100, 0.5)}, {'p', testWAV(48000, 0.5)}}})
	waitFor(t, "both streams", 20*time.Second, func() bool { return len(g.body(0)) > 0 && len(g.body(1)) > 0 && g.count("Play") >= 2 })
	waitFor(t, "the second stream's data", 10*time.Second, func() bool { return len(g.body(1)) >= 44+24000*4-400 })
	if rateOf(g.body(0)) != 44100 || rateOf(g.body(1)) != 48000 {
		t.Errorf("stream rates %d and %d, want 44100 and 48000", rateOf(g.body(0)), rateOf(g.body(1)))
	}
	if n := len(g.body(0)); n < 44+22050*4-400 {
		t.Errorf("the first stream was cut short: %d bytes", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.uriTimes[1].After(g.endTimes[0]) {
		t.Errorf("the second stream started before the renderer had finished the first")
	}
}

// A stream is replaced at a track start once it nears the WAV size limit.
func TestStreamRotation(t *testing.T) {
	trk := testTrack{'p', testWAV(48000, 0.5)} // 96000 bytes of audio each
	g := newRig(t, rigOptions{rotate: 150_000, tracks: []testTrack{trk, trk, trk}})
	waitFor(t, "a second stream", 20*time.Second, func() bool { return len(g.body(1)) >= 44+96000-400 })
	if n0 := len(g.body(0)); n0 < 44+2*96000-400 || n0 > 44+2*96000+400 {
		t.Errorf("the first stream carried %d bytes; two tracks (%d) were expected before rotating", n0, 44+2*96000)
	}
	if g.count("SetAVTransportURI") != 2 {
		t.Errorf("expected exactly 2 streams, got %d", g.count("SetAVTransportURI"))
	}
}

// Without autoresume a renderer that drops the stream pauses LMS; with it a new
// stream is started and LMS never hears about it.
func TestLostStream(t *testing.T) {
	long := []testTrack{{'p', testWAV(48000, 30.0)}} // far more than the pump's window, so it is mid-track when the stream drops
	t.Run("pauses LMS by default", func(t *testing.T) {
		g := newRig(t, rigOptions{tracks: long, dropAfter: 60_000})
		waitFor(t, "LMS to be told it paused", 15*time.Second, func() bool { return g.has("STMp") })
		if g.count("SetAVTransportURI") != 1 {
			t.Errorf("no recovery was asked for, yet %d streams were started", g.count("SetAVTransportURI"))
		}
	})
	t.Run("autoresume starts a new stream", func(t *testing.T) {
		g := newRig(t, rigOptions{tracks: long, dropAfter: 60_000, autoResume: true})
		waitFor(t, "a second stream", 15*time.Second, func() bool { return g.count("SetAVTransportURI") >= 2 })
		time.Sleep(300 * time.Millisecond)
		if g.has("STMp") {
			t.Error("LMS was told the player paused although autoresume was on")
		}
	})
}

// Passthrough serves LMS's own bytes, one URL per track, with the right type.
func TestPassthrough(t *testing.T) {
	mp3 := bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0x00, 1, 2, 3, 4}, 5000) // never decoded
	wav := testWAV(44100, 0.5)
	g := newRig(t, rigOptions{passthrough: true, tracks: []testTrack{{'m', mp3}, {'p', wav}}})
	waitFor(t, "both tracks", 20*time.Second, func() bool { return len(g.body(1)) >= len(wav) })
	if !bytes.Equal(g.body(0), mp3) || !bytes.Equal(g.body(1), wav) {
		t.Errorf("the renderer did not get LMS's bytes unchanged (%d, %d bytes)", len(g.body(0)), len(g.body(1)))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctypes[0] != "audio/mpeg" || g.ctypes[1] != "audio/wav" {
		t.Errorf("content types %q and %q", g.ctypes[0], g.ctypes[1])
	}
	if !strings.HasSuffix(g.uris[0], ".mp3") || !strings.Contains(g.metas[0], "audio/mpeg") {
		t.Errorf("first URL %q, metadata %q", g.uris[0], g.metas[0])
	}
	if !g.uriTimes[1].After(g.endTimes[0]) {
		t.Errorf("the second track started before the renderer had finished the first")
	}
}

// A connected player with nothing playing must not burn CPU: no polling loops.
func TestIdleCPU(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // a fake LMS that accepts the player and says nothing
		if c, err := ln.Accept(); err == nil {
			io.Copy(io.Discard, c)
		}
	}()
	rend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer rend.Close()
	r := &upnp.Renderer{Name: "Fake", IP: net.ParseIP("127.0.0.1"),
		AVTransport: &upnp.Service{Type: "urn:schemas-upnp-org:service:AVTransport:1", ControlURL: rend.URL}}
	finder := &lms.Finder{}
	finder.SetServer(ln.Addr().String(), true)
	c := player.New(player.Options{Name: "T", MAC: net.HardwareAddr{2, 0, 0, 0, 0, 2}, OutRate: 48000,
		Decoder: &decode.Decoder{OutRate: 48000}, Finder: finder})
	out := New(c, r, Options{Name: "T", Idle: 5 * time.Second, VolMax: 100})
	c.SetOutput(out)
	out.Run()
	go c.Loop()
	defer c.Close()

	time.Sleep(500 * time.Millisecond) // let it settle
	start, wall := cpuSeconds(), time.Now()
	time.Sleep(4 * time.Second)
	used := cpuSeconds() - start
	pct := 100 * used / time.Since(wall).Seconds()
	t.Logf("idle CPU: %.1f ms over %.1f s = %.2f%% of one core", used*1000, time.Since(wall).Seconds(), pct)
	if pct > 0.3 {
		t.Errorf("idle CPU %.2f%% of a core; a connected idle player should be near zero", pct)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// In verbose mode the SetVolume log line shows LMS's own volume setting too.
// Verbose is a plain global that the program sets once at start-up. Setting it
// per test would race with goroutines left over from earlier tests.
func init() {
	upnp.Verbose = true
	upnp.NeverFetched = time.Second // the default is 10 s
}

func TestSetVolumeLogShowsLMSVolume(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":1,"result":{"_volume":35}}`))
	}))
	defer web.Close()
	u, _ := url.Parse(web.URL)
	port, _ := net.LookupPort("tcp", u.Port())

	var logs syncBuf
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)

	g := newRig(t, rigOptions{idleOnly: true, volume: true, webPort: port})
	_ = g
	waitFor(t, "the SetVolume line", 10*time.Second, func() bool { return strings.Contains(logs.String(), "UPnP SetVolume") })
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "UPnP SetVolume") {
			line = l
		}
	}
	if !strings.Contains(line, "LMS volume 35%") || !strings.Contains(line, "gain 0.250") || !strings.Contains(line, "[T] UPnP SetVolume 76% ok") {
		t.Errorf("unexpected line: %q", line)
	}
}

// A renderer that never fetches the stream (firewall, wrong network) is reported
// through OnNetFail, once per stream attempt, so a service manager can restart us.
func TestNeverFetchedReportsNetFail(t *testing.T) {
	got := make(chan string, 4)
	newRig(t, rigOptions{tracks: []testTrack{{'p', testWAV(48000, 5)}}, noFetch: true,
		netFail: func(why string) { got <- why }})
	select {
	case why := <-got:
		if !strings.Contains(why, "never fetched") {
			t.Errorf("reason %q", why)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("OnNetFail was not called")
	}
}

// A renderer that fetches normally never triggers it.
func TestFetchedDoesNotReportNetFail(t *testing.T) {
	called := make(chan string, 1)
	g := newRig(t, rigOptions{tracks: []testTrack{{'p', testWAV(48000, 1)}}, netFail: func(s string) { called <- s }})
	waitFor(t, "the track to play", 15*time.Second, func() bool { return len(g.body(0)) > 0 })
	select {
	case why := <-called:
		t.Errorf("OnNetFail called for a healthy renderer: %s", why)
	case <-time.After(2500 * time.Millisecond):
	}
}

// adtsTone is a short AAC-LC (ADTS) stream of a 1 kHz tone.
func adtsTone(t *testing.T, rate int, secs float64) []byte {
	t.Helper()
	var pcm []byte
	for i := 0; i < int(float64(rate)*secs); i++ {
		v := uint16(int16(12000 * math.Sin(2*math.Pi*1000*float64(i)/float64(rate))))
		pcm = binary.LittleEndian.AppendUint16(pcm, v)
		pcm = binary.LittleEndian.AppendUint16(pcm, v)
	}
	var b bytes.Buffer
	if err := aacpcm.EncodeInterleaved(&b, aacpcm.Config{SampleRate: rate, BitDepth: 16, Channels: 2, Bitrate: 128000}, pcm); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// AAC from LMS is decoded in-process, resampled, and sent to the renderer as WAV.
func TestAACDecodedToRenderer(t *testing.T) {
	g := newRig(t, rigOptions{tracks: []testTrack{{'a', adtsTone(t, 44100, 1.5)}}})
	want := 48000 * 4 * 3 / 2 // 1.5 s of 48 kHz stereo 16-bit
	waitFor(t, "the track", 20*time.Second, func() bool { return len(g.body(0)) >= want })
	b := g.body(0)
	if string(b[:4]) != "RIFF" {
		t.Fatalf("not a WAV stream: %q", b[:4])
	}
	if got := binary.LittleEndian.Uint32(b[24:]); got != 48000 {
		t.Errorf("stream rate %d, want 48000 (resampled from 44.1 kHz)", got)
	}
	var peak int16
	for i := 44 + 48000*4/4; i+1 < len(b) && i < 44+48000*4/2; i += 4 {
		if v := int16(binary.LittleEndian.Uint16(b[i:])); v > peak {
			peak = v
		}
	}
	if peak < 10000 || peak > 14000 {
		t.Errorf("tone peak %d, want about 12000", peak)
	}
}

// With decoding off, LMS's AAC goes to the renderer untouched, labelled audio/aac.
func TestAACPassthrough(t *testing.T) {
	aac := adtsTone(t, 44100, 1)
	g := newRig(t, rigOptions{passthrough: true, tracks: []testTrack{{'a', aac}}})
	waitFor(t, "the track", 20*time.Second, func() bool { return len(g.body(0)) >= len(aac) })
	g.mu.Lock()
	defer g.mu.Unlock()
	if !bytes.Equal(g.bodies[0], aac) {
		t.Error("the renderer did not get LMS's bytes unchanged")
	}
	if g.ctypes[0] != "audio/aac" || !strings.HasSuffix(g.uris[0], ".aac") || !strings.Contains(g.metas[0], "audio/aac") {
		t.Errorf("content type %q, URL %q, metadata %q", g.ctypes[0], g.uris[0], g.metas[0])
	}
}

func TestExtensionsForPassthrough(t *testing.T) {
	for mime, want := range map[string]string{"audio/mpeg": "mp3", "audio/flac": "flac", "audio/aiff": "aiff", "audio/wav": "wav",
		"audio/aac": "aac", "audio/mp4": "m4a", "audio/ogg": "ogg"} {
		if got := extFor(mime); got != want {
			t.Errorf("extFor(%q) = %q, want %q", mime, got, want)
		}
	}
}

// Autoresumes are counted, shown in the verbose line between tracks, and the
// state at each loss is printed when the output closes.
func TestRestartSummary(t *testing.T) {
	var logs syncBuf
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)

	long := []testTrack{{'p', testWAV(48000, 30.0)}}
	g := newRig(t, rigOptions{tracks: long, dropAfter: 60_000, autoResume: true})
	waitFor(t, "a second stream", 15*time.Second, func() bool { return g.count("SetAVTransportURI") >= 2 })
	if n := g.out.Resumes(); n < 1 {
		t.Errorf("Resumes() = %d after a lost stream", n)
	}
	g.out.logResumes()
	if !strings.Contains(logs.String(), "autoresumes so far: 1") {
		t.Errorf("no resume count in the log:\n%s", logs.String())
	}
	g.out.Close()
	g.out.Close() // the summary is printed once
	out := logs.String()
	if strings.Count(out, "restart summary") != 1 {
		t.Fatalf("expected one summary:\n%s", out)
	}
	for _, want := range []string{"1 streams lost, 1 autoresumes", "#1 ", "stream 1:", "served", "autoresume true"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// The history keeps only the most recent losses.
func TestRestartHistoryIsBounded(t *testing.T) {
	g := newRig(t, rigOptions{idleOnly: true})
	st := upnp.NewStream(1, upnp.WAVFmt{Rate: 48000, Channels: 2, Bits: 16}, nil)
	for i := 0; i < maxRestarts+25; i++ {
		g.out.recordRestart(st, "test")
	}
	g.out.mu.Lock()
	n, first, lost := len(g.out.history), g.out.history[0].n, g.out.lost
	g.out.mu.Unlock()
	if n != maxRestarts || lost != maxRestarts+25 || first != 26 {
		t.Errorf("kept %d entries, first #%d, %d losses", n, first, lost)
	}
}

// With ICY on, a renderer that asks for metadata gets the track's artist and
// title and its cover art URL (from LMS's web interface) spliced into the stream.
func TestICYMetadata(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":1,"result":{"playlist_tracks":1,"playlist_cur_index":0,"playlist_loop":[{"title":"Song","artist":"Band","coverid":"abc123"}]}}`))
	}))
	defer web.Close()
	u, _ := url.Parse(web.URL)
	port, _ := net.LookupPort("tcp", u.Port())

	g := newRig(t, rigOptions{icy: true, webPort: port, tracks: []testTrack{{'p', testWAV(48000, 8.0)}}})
	find := func() string { // the first non-empty metadata block in what the renderer has received
		body := g.body(0)
		for off := 16000; off < len(body); {
			l := int(body[off]) * 16
			if l > 0 && off+1+l <= len(body) {
				return strings.TrimRight(string(body[off+1:off+1+l]), "\x00")
			}
			off += 1 + l + 16000
		}
		return ""
	}
	// LMS is asked 0.4 s after the track starts, so the title reaches a later block than the first.
	waitFor(t, "a metadata block", 15*time.Second, func() bool { return find() != "" })
	g.mu.Lock()
	metaint := g.metaint
	g.mu.Unlock()
	if metaint != "16000" {
		t.Fatalf("icy-metaint = %q", metaint)
	}
	want := "StreamTitle='Band - Song';StreamUrl='http://127.0.0.1:" + strconv.Itoa(port) + "/music/abc123/cover.jpg';"
	if found := find(); found != want {
		t.Errorf("metadata %q, want %q", found, want)
	}
}
