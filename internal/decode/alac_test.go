package decode

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"testing"
	"time"
)

// makeSource writes a busy test signal as a WAV of the given depth and layout and
// returns the path.
func makeSource(t *testing.T, ff, dir, name string, rate, ch, bits int, secs float64) string {
	t.Helper()
	expr := "0.3*sin(2*PI*440*t)+0.2*sin(2*PI*(300+500*t)*t)+0.1*(random(0)-0.5)"
	if ch == 2 {
		expr += "|0.25*sin(2*PI*1250*t)+0.15*(random(1)-0.5)"
	}
	p := filepath.Join(dir, name+".wav")
	codec := map[int]string{16: "pcm_s16le", 24: "pcm_s24le"}[bits]
	out, err := exec.Command(ff, "-v", "error", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf("aevalsrc=%s:d=%g:s=%d", expr, secs, rate), "-c:a", codec, p).CombinedOutput()
	if err != nil {
		t.Fatalf("making %s: %v\n%s", name, err, out)
	}
	return p
}

// Our own decode of a WAV: the reference for lossless formats (top 16 bits).
func decodeFile(t *testing.T, d *Decoder, format byte, path string) ([]int16, int, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, d, format, data)
}

func TestALACIsLossless(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	cases := []struct {
		name       string
		rate, ch   int
		bits       int
		faststart  bool
		framesSize int
		extra      []string
	}{
		{"stereo 16-bit 44.1k", 44100, 2, 16, false, 0, nil},
		{"stereo 16-bit 44.1k, index first", 44100, 2, 16, true, 0, nil},
		{"stereo 24-bit 48k", 48000, 2, 24, false, 0, nil},
		{"mono 16-bit 44.1k", 44100, 1, 16, false, 0, nil},
		{"mono 24-bit 96k", 96000, 1, 24, false, 0, nil},
		{"stereo 24-bit 192k", 192000, 2, 24, true, 0, nil},
		{"stereo 16-bit 8k", 8000, 2, 16, false, 0, nil},
		{"stereo 16-bit 22.05k, small frames", 22050, 2, 16, false, 1024, nil},
		{"stereo 16-bit, uncompressed frames", 44100, 2, 16, false, 0, []string{"-compression_level", "0"}},
		{"mono 16-bit, uncompressed frames", 44100, 1, 16, false, 0, []string{"-compression_level", "0"}},
		{"stereo 24-bit, uncompressed frames", 48000, 2, 24, false, 0, []string{"-compression_level", "0"}},
		{"mono 24-bit, uncompressed frames", 48000, 1, 24, false, 0, []string{"-compression_level", "0"}},
		{"stereo 16-bit, level 1", 44100, 2, 16, false, 0, []string{"-compression_level", "1"}},
		{"stereo 24-bit, level 2, order 8", 44100, 2, 24, false, 0, []string{"-compression_level", "2", "-min_prediction_order", "8", "-max_prediction_order", "8"}},
		{"mono 16-bit, order 30", 44100, 1, 16, false, 0, []string{"-min_prediction_order", "30", "-max_prediction_order", "30"}},
		{"stereo 16-bit, rice 4", 44100, 2, 16, false, 0, []string{"-min_partition_order", "4", "-max_partition_order", "4"}},
	}
	for i, c := range cases {
		src := makeSource(t, ff, dir, fmt.Sprint("src", i), c.rate, c.ch, c.bits, 4)
		args := []string{"-v", "error", "-y", "-i", src, "-c:a", "alac"}
		if c.bits == 24 {
			args = append(args, "-sample_fmt", "s32p")
		} else {
			args = append(args, "-sample_fmt", "s16p")
		}
		if c.faststart {
			args = append(args, "-movflags", "+faststart")
		}
		args = append(args, c.extra...)
		if c.framesSize > 0 {
			args = append(args, "-frame_size", fmt.Sprint(c.framesSize))
		}
		m4a := filepath.Join(dir, fmt.Sprint("a", i, ".m4a"))
		if out, err := exec.Command(ff, append(args, m4a)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: encoding: %v\n%s", c.name, err, out)
		}
		want, wrate, err := decodeFile(t, &Decoder{}, FormatPCM, src)
		if err != nil {
			t.Fatal(err)
		}
		got, rate, err := decodeFile(t, &Decoder{}, FormatALAC, m4a)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if rate != wrate {
			t.Errorf("%s: rate %d, want %d", c.name, rate, wrate)
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d frames, want %d", c.name, len(got)/2, len(want)/2)
		}
		bad := -1
		for j := 0; j < min(len(got), len(want)); j++ {
			if got[j] != want[j] {
				bad = j
				break
			}
		}
		if bad >= 0 {
			t.Errorf("%s: differs from the source at frame %d (%d vs %d)", c.name, bad/2, got[bad], want[bad])
		} else {
			t.Logf("%-36s bit-exact, %d frames", c.name, len(got)/2)
		}
	}
}

func TestALACProbe(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := makeSource(t, ff, dir, "p", 48000, 2, 24, 2)
	m4a := filepath.Join(dir, "p.m4a")
	if out, err := exec.Command(ff, "-v", "error", "-y", "-i", src, "-c:a", "alac", "-sample_fmt", "s32p", "-movflags", "+faststart", m4a).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	data, _ := os.ReadFile(m4a)
	var info Info
	(&Decoder{}).DecodeInfo(FormatALAC, bytes.NewReader(data), [4]byte{}, func([]int16, int) error { return errStop }, func(i Info) { info = i })
	if info.Codec != "ALAC" || info.Rate != 48000 || info.Channels != 2 || info.Bits != 24 {
		t.Errorf("probe: %+v", info)
	}
	t.Logf("probe: %s", info)
}

// Damaged ALAC and Vorbis files may fail, but must neither panic nor hang.
func TestALACAndVorbisCorruptFilesAreSafe(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	type base struct {
		format byte
		data   []byte
	}
	var bases []base
	mk := func(format byte, name string, args ...string) {
		p := filepath.Join(dir, name)
		a := append([]string{"-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-ac", "2"}, args...)
		if out, err := exec.Command(ff, append(a, p)...).CombinedOutput(); err != nil {
			t.Logf("cannot make %s: %v %s", name, err, out)
			return
		}
		b, _ := os.ReadFile(p)
		bases = append(bases, base{format, b})
	}
	mk(FormatALAC, "a.m4a", "-c:a", "alac")
	mk(FormatALAC, "b.m4a", "-c:a", "alac", "-movflags", "+faststart")
	mk(FormatALAC, "c.m4a", "-c:a", "alac", "-compression_level", "0")
	mk(FormatOgg, "a.ogg", "-c:a", "libvorbis")
	if len(bases) == 0 {
		t.Skip("no test files could be made")
	}
	iters := 200
	if v, err := strconv.Atoi(os.Getenv("AAC_FUZZ_ITER")); err == nil {
		iters = v
	}
	rng := uint32(777)
	next := func() uint32 { rng = rng*1664525 + 1013904223; return rng >> 8 }
	for iter := 0; iter < iters; iter++ {
		bs := bases[iter%len(bases)]
		b := append([]byte(nil), bs.data...)
		switch iter % 3 {
		case 0:
			for k := 0; k < 1+int(next()%8); k++ {
				b[int(next())%len(b)] = byte(next())
			}
		case 1:
			b = b[:int(next())%len(b)]
		case 2:
			for k := 0; k < 6; k++ {
				b[int(next())%min(len(b), 4096)] = byte(next())
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("iteration %d (format %q) panicked: %v\n%s", iter, bs.format, r, debug.Stack())
				}
			}()
			(&Decoder{}).Decode(bs.format, bytes.NewReader(b), [4]byte{}, func([]int16, int) error { return nil })
			Probe(bs.format, b[:min(len(b), 64<<10)], [4]byte{})
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("iteration %d (format %q) hung", iter, bs.format)
		}
	}
}
