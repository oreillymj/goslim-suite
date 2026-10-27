package decode

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func timeNow() time.Time              { return time.Now() }
func since(t time.Time) time.Duration { return time.Since(t) }

// mp3Frames builds n consecutive MPEG-1 Layer III frames of the given header bytes.
func mp3Frames(hdr [4]byte, frameLen, n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		f := make([]byte, frameLen)
		copy(f, hdr[:])
		b.Write(f)
	}
	return b.Bytes()
}

func TestProbeMP3(t *testing.T) {
	// FF FB 90 00 = MPEG-1 Layer III, 128 kbps, 44.1 kHz, stereo; frames are 417 bytes
	cbr := mp3Frames([4]byte{0xFF, 0xFB, 0x90, 0x00}, 417, 6)
	info, ok := Probe(FormatMP3, cbr, [4]byte{})
	if !ok || !strings.Contains(info.String(), "MPEG-1 Layer III") || !strings.Contains(info.String(), "128 kbps CBR") || info.Rate != 44100 {
		t.Errorf("plain CBR: %v %q", ok, info)
	}

	// a frame whose bitrate changes is VBR: 192 kbps frames are 626 bytes
	vbr := append(mp3Frames([4]byte{0xFF, 0xFB, 0x90, 0x00}, 417, 1), mp3Frames([4]byte{0xFF, 0xFB, 0xB0, 0x00}, 626, 4)...)
	if info, _ := Probe(FormatMP3, vbr, [4]byte{}); !strings.Contains(info.String(), "VBR") {
		t.Errorf("changing bitrate should read as VBR: %q", info)
	}

	// Xing header: VBR with 1000 frames in 500000 bytes = average 152 kbps at 44.1 kHz
	xing := mp3Frames([4]byte{0xFF, 0xFB, 0x90, 0x00}, 417, 4)
	copy(xing[4+32:], "Xing")
	binary.BigEndian.PutUint32(xing[4+32+4:], 3)
	binary.BigEndian.PutUint32(xing[4+32+8:], 1000)
	binary.BigEndian.PutUint32(xing[4+32+12:], 500000)
	if info, _ := Probe(FormatMP3, xing, [4]byte{}); !strings.Contains(info.String(), "VBR avg 15") {
		t.Errorf("Xing header: %q", info)
	}
	// LAME's "Info" header marks CBR
	copy(xing[4+32:], "Info")
	if info, _ := Probe(FormatMP3, xing, [4]byte{}); !strings.Contains(info.String(), "128 kbps CBR") {
		t.Errorf("Info header: %q", info)
	}

	// an ID3v2 tag in front is skipped
	tag := append([]byte("ID3\x04\x00\x00\x00\x00\x01\x00"), make([]byte, 128)...) // size 128
	if info, ok := Probe(FormatMP3, append(tag, cbr...), [4]byte{}); !ok || info.Rate != 44100 {
		t.Errorf("after an ID3 tag: %v %q", ok, info)
	}
	// MPEG-2 at 24 kHz, mono: FF F3 = MPEG-2 L3; 0x50 = 64 kbps? index 5 -> 40 kbps; 0x14 sample index 1 -> 24000
	if info, _ := Probe(FormatMP3, mp3Frames([4]byte{0xFF, 0xF3, 0x54, 0xC0}, 72000*40/24000, 5), [4]byte{}); info.Rate != 24000 || info.Channels != 1 || !strings.Contains(info.String(), "MPEG-2 Layer III") {
		t.Errorf("MPEG-2 mono: %q", info)
	}
}

func TestProbeFLACAndPCM(t *testing.T) {
	// fLaC + STREAMINFO block header + body: 96 kHz, 2 channels, 24 bit, 960000 samples
	b := append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 34)...)
	v := uint64(96000)<<44 | uint64(1)<<41 | uint64(23)<<36 | 960000
	binary.BigEndian.PutUint64(b[18:], v)
	info, ok := Probe(FormatFLAC, b, [4]byte{})
	if !ok || info.Rate != 96000 || info.Channels != 2 || info.Bits != 24 || !strings.Contains(info.String(), "FLAC") || !strings.Contains(info.String(), "0:10") {
		t.Errorf("FLAC: %v %q", ok, info)
	}
	if info, _ := Probe(FormatPCM, wavFile(tone(44100, 100, 1000, false), 44100, 0xFFFFFFFF), [4]byte{}); info.Codec != "WAV" || info.Rate != 44100 || info.Bits != 16 {
		t.Errorf("WAV: %q", info)
	}
	if info, _ := Probe(FormatPCM, aiffFile(tone(48000, 100, 1000, false), 48000), [4]byte{}); info.Codec != "AIFF" || info.Rate != 48000 {
		t.Errorf("AIFF: %q", info)
	}
	if info, _ := Probe(FormatPCM, []byte{1, 2, 3}, [4]byte{'1', '3', '2', '1'}); info.Codec != "PCM" || info.Rate != 44100 {
		t.Errorf("raw PCM: %q", info)
	}
}

func TestDecodeInfoReportsBeforeAudio(t *testing.T) {
	var order []string
	d := &Decoder{OutRate: 48000}
	err := d.DecodeInfo(FormatPCM, bytes.NewReader(wavFile(tone(44100, 500, 1000, false), 44100, 0xFFFFFFFF)), [4]byte{},
		func(s []int16, r int) error { order = append(order, "audio"); return nil },
		func(i Info) { order = append(order, "info:"+i.String()) })
	if err != nil || len(order) < 2 || !strings.HasPrefix(order[0], "info:WAV") || order[1] != "audio" {
		t.Errorf("order %v, err %v", order, err)
	}
}
