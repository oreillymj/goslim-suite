package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	aacpcm "github.com/tphakala/go-aac/pcm"
)

// collect runs the decoder and returns its stereo output and sample rate.
func collect(t *testing.T, d *Decoder, format byte, data []byte) ([]int16, int, error) {
	t.Helper()
	var out []int16
	rate := 0
	err := d.Decode(format, bytes.NewReader(data), [4]byte{}, func(s []int16, r int) error {
		if rate != 0 && r != rate {
			t.Errorf("rate changed from %d to %d inside one stream", rate, r)
		}
		rate = r
		out = append(out, s...)
		return nil
	})
	return out, rate, err
}

// A stream made by go-aac's own encoder decodes to the tone that went in.
func TestAACADTSRoundTrip(t *testing.T) {
	const rate, n = 44100, 44100 * 2
	pcm := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*1000*float64(i)/rate))
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v/2))
	}
	var adts bytes.Buffer
	if err := aacpcm.EncodeInterleaved(&adts, aacpcm.Config{SampleRate: rate, BitDepth: 16, Channels: 2, Bitrate: 128000}, pcm); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"adts":             adts.Bytes(),
		"adts after ID3v2": append([]byte("ID3\x03\x00\x00\x00\x00\x00\x0c0123456789ab"), adts.Bytes()...),
	} {
		got, r, err := collect(t, &Decoder{}, FormatAAC, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r != rate || len(got) < n*2 {
			t.Fatalf("%s: rate %d, %d samples (want %d, at least %d)", name, r, len(got), rate, n*2)
		}
		// RMS of a 12000-peak sine is 8485; the right channel is half of that.
		var l, rr float64
		for i := 20000; i < 60000; i++ {
			l += float64(got[2*i]) * float64(got[2*i])
			rr += float64(got[2*i+1]) * float64(got[2*i+1])
		}
		l, rr = math.Sqrt(l/40000), math.Sqrt(rr/40000)
		if math.Abs(l-8485) > 400 || math.Abs(rr-4243) > 250 {
			t.Errorf("%s: channel RMS %.0f / %.0f, want about 8485 / 4243", name, l, rr)
		}
	}
	if info, ok := Probe(FormatAAC, adts.Bytes(), [4]byte{}); !ok || info.Rate != rate || info.Channels != 2 || !strings.HasPrefix(info.Detail, "LC") {
		t.Errorf("probe: %+v %v", info, ok)
	}
}

func TestAACGarbageIsAnError(t *testing.T) {
	if _, _, err := collect(t, &Decoder{}, FormatAAC, bytes.Repeat([]byte("not audio at all "), 500)); err == nil {
		t.Error("expected an error for non-AAC data")
	}
	// A truncated MP4 header must not panic or hang.
	for _, n := range []int{0, 3, 11, 40} {
		collect(t, &Decoder{}, FormatAAC, append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, make([]byte, n)...))
	}
}

// ffmpegPCM decodes a file with ffmpeg's bit-exact fixed-point AAC decoder.
func ffmpegPCM(t *testing.T, ff, path string, ch int) []int16 {
	t.Helper()
	out, err := exec.Command(ff, "-v", "error", "-c:a", "aac_fixed", "-i", path, "-f", "s16le", "-ac", fmt.Sprint(ch), "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg decode of %s: %v", path, err)
	}
	s := make([]int16, len(out)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(out[2*i:]))
	}
	return s
}

func TestAACMatchesFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	// A busy test signal: two tones, a sweep and noise, different on each side.
	src := "aevalsrc=0.3*sin(2*PI*440*t)+0.2*sin(2*PI*(300+500*t)*t)+0.1*(random(0)-0.5)|0.25*sin(2*PI*1250*t)+0.15*(random(1)-0.5):d=6:s=44100"
	mk := func(name string, args ...string) string {
		p := filepath.Join(dir, name)
		a := append([]string{"-v", "error", "-y", "-f", "lavfi", "-i", src}, args...)
		a = append(a, p)
		if out, err := exec.Command(ff, a...).CombinedOutput(); err != nil {
			t.Fatalf("making %s: %v\n%s", name, err, out)
		}
		return p
	}
	cases := []struct {
		name string
		path string
		ch   int
		rate int
	}{
		{"adts stereo 44.1k", mk("a.aac", "-c:a", "aac", "-b:a", "128k"), 2, 44100},
		{"adts with ID3v2", mk("id3.aac", "-c:a", "aac", "-b:a", "96k", "-write_id3v2", "1", "-metadata", "title=Test"), 2, 44100},
		{"m4a, index at the end", mk("end.m4a", "-c:a", "aac", "-b:a", "192k"), 2, 44100},
		{"m4a, index first", mk("front.m4a", "-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart"), 2, 44100},
		{"m4a mono 48k", mk("mono.m4a", "-c:a", "aac", "-ac", "1", "-ar", "48000", "-b:a", "64k"), 1, 48000},
		{"m4a 22.05k", mk("lo.m4a", "-c:a", "aac", "-ar", "22050", "-b:a", "48k"), 2, 22050},
		{"m4a 96k VBR-ish", mk("vbr.m4a", "-c:a", "aac", "-q:a", "2"), 2, 44100},
	}
	for _, c := range cases {
		data, _ := os.ReadFile(c.path)
		got, rate, err := collect(t, &Decoder{}, FormatAAC, data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := ffmpegPCM(t, ff, c.path, c.ch)
		if c.ch == 1 { // ffmpeg writes mono; we duplicate it
			st := make([]int16, 0, len(want)*2)
			for _, v := range want {
				st = append(st, v, v)
			}
			want = st
		}
		if rate != c.rate {
			t.Errorf("%s: rate %d, want %d", c.name, rate, c.rate)
		}
		// ffmpeg does not trim the encoder padding at the end of an m4a; the edit
		// list says where the audio really ends (6 s here), and we honour it.
		if strings.HasSuffix(c.path, ".m4a") {
			if frames := len(got) / 2; frames != 6*c.rate {
				t.Errorf("%s: %d frames, want exactly 6 s = %d", c.name, frames, 6*c.rate)
			}
			if len(got) > len(want) {
				t.Errorf("%s: longer than ffmpeg's output", c.name)
			}
		} else if len(got) != len(want) {
			t.Errorf("%s: %d samples, ffmpeg gives %d", c.name, len(got), len(want))
		}
		n := min(len(got), len(want))
		maxd := 0
		for i := 0; i < n; i++ {
			if d := int(got[i]) - int(want[i]); d > maxd {
				maxd = d
			} else if -d > maxd {
				maxd = -d
			}
		}
		if maxd > 1 {
			t.Errorf("%s: differs from ffmpeg by up to %d LSB", c.name, maxd)
		}
		t.Logf("%-24s %d Hz, %d frames, max diff vs ffmpeg %d LSB", c.name, rate, len(got)/2, maxd)

		var info Info
		d := &Decoder{}
		d.DecodeInfo(FormatAAC, bytes.NewReader(data), [4]byte{}, func([]int16, int) error { return errStop }, func(i Info) { info = i })
		if strings.Contains(info.Detail, "index at the end") {
			t.Logf("%-24s probe: %s", "", info) // the index is past what the probe sees
		} else if info.Rate != c.rate || info.Channels != c.ch {
			t.Errorf("%s: probe says %+v", c.name, info)
		} else {
			t.Logf("%-24s probe: %s", "", info)
		}
	}
}

var errStop = fmt.Errorf("stop")

// LATM is not decoded natively; without ffmpeg that is a clear error, with it the
// stream is handed over from the start.
func TestAACFallbackToFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.latm")
	if out, err := exec.Command(ff, "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-ac", "2", "-c:a", "aac", "-f", "latm", p).CombinedOutput(); err != nil {
		t.Skipf("cannot make LATM: %v %s", err, out)
	}
	data, _ := os.ReadFile(p)
	if _, _, err := collect(t, &Decoder{}, FormatAAC, data); err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("without ffmpeg expected an error naming ffmpeg, got %v", err)
	}
	got, rate, err := collect(t, &Decoder{FFmpeg: ff, OutRate: 44100}, FormatAAC, data)
	if err != nil || len(got) < 44100*2 || rate != 44100 {
		t.Errorf("with ffmpeg: %d samples at %d Hz, err %v", len(got), rate, err)
	}
}

// Damaged or hostile files may fail, but must neither panic, hang nor eat memory.
func TestAACCorruptFilesAreSafe(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	var bases [][]byte
	for _, fast := range []bool{false, true} {
		p := filepath.Join(dir, fmt.Sprint(fast, ".m4a"))
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-ac", "2", "-c:a", "aac"}
		if fast {
			args = append(args, "-movflags", "+faststart")
		}
		if out, err := exec.Command(ff, append(args, p)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		b, _ := os.ReadFile(p)
		bases = append(bases, b)
	}
	rng := uint32(12345)
	next := func() uint32 { rng = rng*1664525 + 1013904223; return rng >> 8 }
	iters := 150 // AAC_FUZZ_ITER=20000 for a long run
	if v, err := strconv.Atoi(os.Getenv("AAC_FUZZ_ITER")); err == nil {
		iters = v
	}
	for iter := 0; iter < iters; iter++ {
		b := append([]byte(nil), bases[iter%2]...)
		switch iter % 3 {
		case 0: // flip a few bytes anywhere
			for k := 0; k < 1+int(next()%8); k++ {
				b[int(next())%len(b)] = byte(next())
			}
		case 1: // truncate
			b = b[:int(next())%len(b)]
		case 2: // damage the first 2 KB, where the boxes and tables live
			for k := 0; k < 6; k++ {
				b[int(next())%min(len(b), 2048)] = byte(next())
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("iteration %d panicked: %v\n%s", iter, r, debug.Stack())
				}
			}()
			(&Decoder{}).Decode(FormatAAC, bytes.NewReader(b), [4]byte{}, func([]int16, int) error { return nil })
			Probe(FormatAAC, b[:min(len(b), 64<<10)], [4]byte{})
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("iteration %d hung", iter)
		}
	}
}
