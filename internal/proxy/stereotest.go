package proxy

// -stereotest <file.wav>: when a GoUPnPAudioClient connects to the proxy port,
// stream this WAV file to it (once) instead of connecting to LMS. Use a file
// with distinct left and right channels to check what a speaker group plays.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"goslim/internal/decode"
	"goslim/internal/proxyproto"
	"goslim/internal/upnp"
)

// StereoTest is a WAV file converted to 16-bit stereo at the output rate, streamed to each proxy
// client instead of LMS audio. Use a file with distinct left and right channels.
type StereoTest struct {
	pcm  []byte
	name string
}

// parseWAVFile extracts PCM audio from a RIFF/WAVE file.
func parseWAVFile(b []byte) (rate, ch, bits int, pcm []byte, err error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, 0, nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	le := binary.LittleEndian
	haveFmt := false
	for pos := 12; pos+8 <= len(b); {
		id := string(b[pos : pos+4])
		size := int(le.Uint32(b[pos+4:]))
		body := pos + 8
		end := body + size
		if end > len(b) || end < body {
			end = len(b)
		}
		switch id {
		case "fmt ":
			if end-body < 16 {
				return 0, 0, 0, nil, fmt.Errorf("short fmt chunk")
			}
			tag := int(le.Uint16(b[body:]))
			ch = int(le.Uint16(b[body+2:]))
			rate = int(le.Uint32(b[body+4:]))
			bits = int(le.Uint16(b[body+14:]))
			if tag == 0xFFFE && end-body >= 26 {
				tag = int(le.Uint16(b[body+24:])) // WAVE_FORMAT_EXTENSIBLE sub-format
			}
			if tag != 1 {
				return 0, 0, 0, nil, fmt.Errorf("only PCM WAV files are supported (format tag %d)", tag)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return 0, 0, 0, nil, fmt.Errorf("data chunk before fmt chunk")
			}
			return rate, ch, bits, b[body:end], nil
		}
		pos = body + size + size&1
	}
	return 0, 0, 0, nil, fmt.Errorf("no data chunk")
}

// LoadStereoTest reads the file (next to the executable, else as given) and
// converts it to 16-bit stereo at the output rate.
func LoadStereoTest(name string, outRate int) (*StereoTest, error) {
	path := name
	if !filepath.IsAbs(name) {
		if exe, err := os.Executable(); err == nil {
			if p := filepath.Join(filepath.Dir(exe), name); fileExists(p) {
				path = p
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rate, ch, bits, raw, err := parseWAVFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if ch < 1 || (bits != 8 && bits != 16 && bits != 24 && bits != 32) {
		return nil, fmt.Errorf("%s: unsupported format (%d channels, %d bit)", path, ch, bits)
	}
	bps := bits / 8
	frame := ch * bps
	n := len(raw) / frame
	sample := func(off int) int16 {
		switch bits {
		case 8:
			return int16(int(raw[off])-128) << 8
		case 16:
			return int16(binary.LittleEndian.Uint16(raw[off:]))
		case 24:
			return int16(int32(raw[off+1]) | int32(int8(raw[off+2]))<<8)
		}
		return int16(binary.LittleEndian.Uint32(raw[off:]) >> 16) // 32 bit
	}
	st := make([]int16, 0, n*2)
	for i := 0; i < n; i++ {
		l := sample(i * frame)
		r := l
		if ch > 1 {
			r = sample(i*frame + bps)
		}
		st = append(st, l, r)
	}
	if rate != outRate {
		st = decode.NewResampler(rate, outRate).Process(st)
	}
	out := make([]byte, len(st)*2)
	for i, v := range st {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	log.Printf("stereo test file %s: %d ch, %d Hz, %d bit, %.1f s (sent as 2 ch at %d Hz)",
		path, ch, rate, bits, float64(n)/float64(rate), outRate)
	if ch < 2 {
		log.Printf("warning: the test file is mono, so both channels will be identical")
	}
	return &StereoTest{pcm: out, name: path}, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// runStereoTest serves one proxy client: it plays the test file once.
func (st *StereoTest) serve(nc net.Conn, h proxyproto.Hello, rate int) {
	p := &conn{nc: nc, name: h.Name}
	p.state.Store("stopped")
	log.Printf("stereo test: %q (%s) connected from %s, streaming %s", h.Name, h.MAC, nc.RemoteAddr(), st.name)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			t, payload, err := proxyproto.ReadFrame(nc)
			if err != nil {
				return
			}
			if t != proxyproto.FrameStatus {
				continue
			}
			var s proxyproto.Status
			if json.Unmarshal(payload, &s) != nil {
				continue
			}
			p.consumed.Store(s.Consumed) // the client counts from zero on every connection
			if old, _ := p.state.Swap(s.State).(string); old != s.State {
				log.Printf("stereo test: renderer state: %s", s.State)
			}
		}
	}()

	p.send(proxyproto.FrameTrack, upnp.WAVHeader(rate))
	for off := 0; off < len(st.pcm); {
		select {
		case <-done:
			return
		default:
		}
		if p.Inflight() >= window {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		n := min(chunk, len(st.pcm)-off)
		p.sent.Add(int64(n))
		p.send(proxyproto.FrameData, st.pcm[off:off+n])
		off += n
	}
	p.send(proxyproto.FrameEnd, nil)
	log.Printf("stereo test: whole file sent")
	for p.consumed.Load() < p.sent.Load() {
		select {
		case <-done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	log.Printf("stereo test: the renderer has pulled the whole file")
	<-done
	log.Printf("stereo test: %q disconnected", h.Name)
}
