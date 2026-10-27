package decode

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func tone(rate, n int, amp float64, leftOnly bool) []int16 {
	out := make([]int16, 0, n*2)
	for i := 0; i < n; i++ {
		v := int16(amp * math.Sin(2*math.Pi*1000*float64(i)/float64(rate)))
		r := v
		if leftOnly {
			r = 0
		}
		out = append(out, v, r)
	}
	return out
}

func rms(xs []int16, ch int) float64 {
	var sum float64
	var n int
	for i := ch; i < len(xs); i += 2 {
		sum += float64(xs[i]) * float64(xs[i])
		n++
	}
	return math.Sqrt(sum / float64(n))
}

func TestResamplerGainAndChannels(t *testing.T) {
	for _, c := range [][2]int{{44100, 48000}, {96000, 48000}, {48000, 44100}} {
		in := tone(c[0], c[0]/4, 10000, true)
		out := NewResampler(c[0], c[1]).Process(in)
		mid := out[400 : len(out)-400]
		if g := rms(mid, 0) / rms(in[400:len(in)-400], 0); g < 0.99 || g > 1.01 {
			t.Errorf("%d->%d: gain %.4f", c[0], c[1], g)
		}
		if r := rms(mid, 1); r > 1 {
			t.Errorf("%d->%d: right channel leaked, rms %.2f", c[0], c[1], r)
		}
	}
}

func TestResamplerChunkedEqualsWhole(t *testing.T) {
	in := tone(44100, 20000, 12000, false)
	whole := NewResampler(44100, 48000).Process(in)
	rs := NewResampler(44100, 48000)
	var chunked []int16
	for i := 0; i < len(in); i += 4096 {
		end := i + 4096
		if end > len(in) {
			end = len(in)
		}
		chunked = append(chunked, rs.Process(in[i:end])...)
	}
	n := len(chunked)
	if len(whole) < n {
		n = len(whole)
	}
	if !bytes.Equal(int16Bytes(whole[:n]), int16Bytes(chunked[:n])) {
		t.Error("chunked output differs from one-shot output")
	}
}

func int16Bytes(xs []int16) []byte {
	b := make([]byte, len(xs)*2)
	for i, v := range xs {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func wavFile(samples []int16, rate int, dataSize uint32) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	binary.Write(&b, le, uint32(0xFFFFFFFF))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, le, uint32(16))
	binary.Write(&b, le, uint16(1))
	binary.Write(&b, le, uint16(2))
	binary.Write(&b, le, uint32(rate))
	binary.Write(&b, le, uint32(rate*4))
	binary.Write(&b, le, uint16(4))
	binary.Write(&b, le, uint16(16))
	b.WriteString("LIST")
	binary.Write(&b, le, uint32(4)) // an extra chunk that must be skipped
	b.WriteString("INFO")
	b.WriteString("data")
	binary.Write(&b, le, dataSize)
	b.Write(int16Bytes(samples))
	return b.Bytes()
}

func aiffFile(samples []int16, rate int) []byte {
	var b bytes.Buffer
	be := binary.BigEndian
	b.WriteString("FORM")
	binary.Write(&b, be, uint32(0))
	b.WriteString("AIFF")
	b.WriteString("COMM")
	binary.Write(&b, be, uint32(18))
	binary.Write(&b, be, uint16(2))
	binary.Write(&b, be, uint32(len(samples)/2))
	binary.Write(&b, be, uint16(16))
	// 80-bit extended float sample rate
	exp := 0
	for (1 << (exp + 1)) <= rate {
		exp++
	}
	mant := uint64(rate) << (63 - exp)
	binary.Write(&b, be, uint16(16383+exp))
	binary.Write(&b, be, mant)
	b.WriteString("SSND")
	binary.Write(&b, be, uint32(8+len(samples)*2))
	binary.Write(&b, be, uint32(0))
	binary.Write(&b, be, uint32(0))
	for _, v := range samples {
		binary.Write(&b, be, v)
	}
	return b.Bytes()
}

func decodePCM(t *testing.T, data []byte, hint [4]byte) ([]int16, int) {
	t.Helper()
	var got []int16
	rate := 0
	d := &Decoder{OutRate: 48000}
	err := d.Decode(FormatPCM, bytes.NewReader(data), hint, func(s []int16, r int) error {
		got = append(got, s...)
		rate = r
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, rate
}

func TestPCMFormats(t *testing.T) {
	want := tone(44100, 1000, 9000, false)
	check := func(name string, got []int16, rate int) {
		if rate != 44100 || len(got) != len(want) {
			t.Fatalf("%s: rate %d, %d samples (want 44100, %d)", name, rate, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: sample %d is %d, want %d", name, i, got[i], want[i])
			}
		}
	}
	g, r := decodePCM(t, wavFile(want, 44100, 0xFFFFFFFF), [4]byte{})
	check("wav (streamed length)", g, r)
	g, r = decodePCM(t, wavFile(want, 44100, uint32(len(want)*2)), [4]byte{})
	check("wav (exact length)", g, r)
	g, r = decodePCM(t, aiffFile(want, 44100), [4]byte{})
	check("aiff", g, r)
	g, r = decodePCM(t, int16Bytes(want), [4]byte{'1', '3', '2', '1'}) // 16 bit, 44.1k, stereo, little endian
	check("raw pcm", g, r)
}

func TestSupportedFormats(t *testing.T) {
	d := &Decoder{}
	for _, f := range []byte{FormatMP3, FormatFLAC, FormatPCM, FormatAAC, FormatALAC, FormatOgg} {
		if !d.Supported(f) {
			t.Errorf("format %q should be supported", f)
		}
	}
	if d.Caps() != "flc,pcm,mp3,aac,alc,ogg" {
		t.Errorf("caps %q", d.Caps())
	}
	// Each native decoder can be switched off; ffmpeg then takes AAC and ALAC if it is there.
	off := &Decoder{NoNativeAAC: true, NoNativeALAC: true, NoNativeVorbis: true}
	if off.Supported(FormatAAC) || off.Supported(FormatALAC) || off.Supported(FormatOgg) || off.Caps() != "flc,pcm,mp3" {
		t.Errorf("with the native decoders off and no ffmpeg only the basics remain: %q", off.Caps())
	}
	off.FFmpeg = "ffmpeg"
	if !off.Supported(FormatAAC) || !off.Supported(FormatALAC) || off.Supported(FormatOgg) || off.Caps() != "flc,pcm,mp3,aac,alc" {
		t.Errorf("with ffmpeg and the native decoders off: %q", off.Caps())
	}
}

// The polyphase fast path must agree with the general path to within rounding.
func TestPolyphaseMatchesGeneralPath(t *testing.T) {
	for _, c := range [][2]int{{44100, 48000}, {96000, 48000}, {48000, 44100}, {88200, 48000}} {
		in := tone(c[0], c[0]/2, 12000, false)
		fast := NewResampler(c[0], c[1]).Process(in)
		slow := (&Resampler{step: float64(c[0]) / float64(c[1])}).Process(in)
		n := len(fast)
		if len(slow) < n {
			n = len(slow)
		}
		if d := len(fast) - len(slow); d < -2 || d > 2 {
			t.Errorf("%d->%d: %d vs %d samples", c[0], c[1], len(fast), len(slow))
		}
		var maxd int
		for i := 0; i < n; i++ {
			d := int(fast[i]) - int(slow[i])
			if d < 0 {
				d = -d
			}
			if d > maxd {
				maxd = d
			}
		}
		if maxd > 2 {
			t.Errorf("%d->%d: fast and general paths differ by up to %d LSB", c[0], c[1], maxd)
		}
	}
}

func TestResamplerSpeed(t *testing.T) {
	in := tone(44100, 44100*10, 12000, false)
	time := func(r *Resampler) float64 {
		start := timeNow()
		for i := 0; i < len(in); i += 8192 {
			end := i + 8192
			if end > len(in) {
				end = len(in)
			}
			r.Process(in[i:end])
		}
		return since(start).Seconds()
	}
	fast := time(NewResampler(44100, 48000))
	slow := time(&Resampler{step: 44100.0 / 48000})
	t.Logf("44.1 -> 48 kHz, 10 s of audio: polyphase %.1f ms, general %.1f ms (%.1fx faster)", fast*1000, slow*1000, slow/fast)
	if fast >= slow {
		t.Errorf("the polyphase path should be faster")
	}
}

func TestContentTypes(t *testing.T) {
	mp4 := []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'M', '4', 'A', ' ', 0, 0, 0, 0}
	for _, c := range []struct {
		format byte
		head   []byte
		want   string
	}{
		{FormatMP3, nil, "audio/mpeg"},
		{FormatFLAC, nil, "audio/flac"},
		{FormatPCM, []byte("RIFF"), "audio/wav"},
		{FormatPCM, []byte("FORM"), "audio/aiff"},
		{FormatAAC, []byte{0xff, 0xf1, 0x50, 0x80}, "audio/aac"},
		{FormatAAC, mp4, "audio/mp4"},
		{FormatALAC, mp4, "audio/mp4"},
		{FormatOgg, []byte("OggS"), "audio/ogg"},
	} {
		if got := ContentType(c.format, c.head); got != c.want {
			t.Errorf("ContentType(%q) = %q, want %q", c.format, got, c.want)
		}
	}
}
