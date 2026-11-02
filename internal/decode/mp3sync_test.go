package decode

import (
	"bytes"
	"io"
	"testing"
)

func mp3TestFrames(n int) []byte {
	// MPEG-1 Layer III, 128 kbps, 44.1 kHz, no padding: 417 bytes per frame.
	var b []byte
	for i := 0; i < n; i++ {
		f := make([]byte, 417)
		copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
		for j := 4; j < len(f); j++ {
			f[j] = byte(j * 31)
		}
		b = append(b, f...)
	}
	return b
}

func TestMP3SyncSkipsToFrame(t *testing.T) {
	frames := mp3TestFrames(5)
	// Starts in the middle of a frame, with stray sync-like bytes (a Layer II look-alike) before the next frame.
	junk := append(bytes.Repeat([]byte{0x12, 0x34}, 50), 0xFF, 0xFD, 0x90, 0x00, 0x55)
	in := append(append([]byte(nil), junk...), frames...)
	got, _ := io.ReadAll(mp3Sync(bytes.NewReader(in)))
	if !bytes.Equal(got, frames) {
		t.Fatalf("got %d bytes, want %d starting at the first real frame", len(got), len(frames))
	}
	// Already aligned, and ID3-tagged streams, are untouched.
	if got, _ := io.ReadAll(mp3Sync(bytes.NewReader(frames))); !bytes.Equal(got, frames) {
		t.Error("aligned stream changed")
	}
	id3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), frames...)
	if got, _ := io.ReadAll(mp3Sync(bytes.NewReader(id3))); !bytes.Equal(got, id3) {
		t.Error("ID3 stream changed")
	}
}
