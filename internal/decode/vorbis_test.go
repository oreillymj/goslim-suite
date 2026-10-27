package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVorbisMatchesFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command(ff, "-hide_banner", "-encoders").Output(); !bytes.Contains(out, []byte("libvorbis")) {
		t.Skip("this ffmpeg has no libvorbis encoder")
	}
	dir := t.TempDir()
	cases := []struct {
		name     string
		rate, ch int
		args     []string
	}{
		{"stereo 44.1k q5", 44100, 2, []string{"-q:a", "5"}},
		{"stereo 48k q10", 48000, 2, []string{"-q:a", "10"}},
		{"stereo 44.1k q-1", 44100, 2, []string{"-q:a", "-1"}},
		{"stereo 44.1k 128 kbps", 44100, 2, []string{"-b:a", "128k"}},
		{"mono 44.1k q4", 44100, 1, []string{"-q:a", "4"}},
		{"stereo 96k q6", 96000, 2, []string{"-q:a", "6"}},
		{"stereo 22.05k q3", 22050, 2, []string{"-q:a", "3"}},
		{"stereo 8k q3", 8000, 2, []string{"-q:a", "3"}},
	}
	for i, c := range cases {
		src := makeSource(t, ff, dir, fmt.Sprint("v", i), c.rate, c.ch, 16, 4)
		ogg := filepath.Join(dir, fmt.Sprint("v", i, ".ogg"))
		if out, err := exec.Command(ff, append([]string{"-v", "error", "-y", "-i", src, "-c:a", "libvorbis"}, append(c.args, ogg)...)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		raw, err := exec.Command(ff, "-v", "error", "-i", ogg, "-f", "s16le", "-ac", fmt.Sprint(c.ch), "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		want := make([]int16, 0, len(raw))
		for j := 0; j+1 < len(raw); j += 2 * c.ch {
			l := int16(binary.LittleEndian.Uint16(raw[j:]))
			r := l
			if c.ch == 2 {
				r = int16(binary.LittleEndian.Uint16(raw[j+2:]))
			}
			want = append(want, l, r)
		}
		got, rate, err := decodeFile(t, &Decoder{}, FormatOgg, ogg)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if rate != c.rate {
			t.Errorf("%s: rate %d, want %d", c.name, rate, c.rate)
		}
		maxd := 0
		for j := 0; j < min(len(got), len(want)); j++ {
			d := int(got[j]) - int(want[j])
			if d < 0 {
				d = -d
			}
			maxd = max(maxd, d)
		}
		t.Logf("%-24s %d frames (ffmpeg %d), max diff %d LSB", c.name, len(got)/2, len(want)/2, maxd)
		if len(got) != len(want) {
			t.Errorf("%s: %d frames, ffmpeg gives %d", c.name, len(got)/2, len(want)/2)
		}
		if maxd > 2 {
			t.Errorf("%s: differs from ffmpeg by up to %d LSB", c.name, maxd)
		}
		data, _ := os.ReadFile(ogg)
		var info Info
		(&Decoder{}).DecodeInfo(FormatOgg, bytes.NewReader(data), [4]byte{}, func([]int16, int) error { return errStop }, func(i Info) { info = i })
		if info.Codec != "Vorbis" || info.Rate != c.rate || info.Channels != c.ch {
			t.Errorf("%s: probe says %+v", c.name, info)
		} else if i == 0 {
			t.Logf("probe: %s", info)
		}
	}
}

func TestVorbisNotVorbisIsAnError(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	p := filepath.Join(t.TempDir(), "o.opus")
	if out, err := exec.Command(ff, "-v", "error", "-y", "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "libopus", p).CombinedOutput(); err != nil {
		t.Skipf("no opus encoder: %v %s", err, out)
	}
	if _, _, err := decodeFile(t, &Decoder{}, FormatOgg, p); err == nil || !strings.Contains(err.Error(), "vorbis") {
		t.Errorf("expected a vorbis error for an Opus stream, got %v", err)
	}
}
