package decode

import (
	"bufio"
	"io"
)

// mp3Sync returns a reader that starts at the first MPEG audio Layer III frame in r. LMS can start a
// stream in the middle of a file (after a restart, a seek, or when the player reconnects), and go-mp3
// gives up when the first bytes are not a frame header ("only layer3 (want 1; got 2)", for instance,
// when the first byte pair happens to look like a Layer II header). A header is accepted only when
// the next frame begins where this one says it ends, so stray 0xFF bytes in audio data are skipped.
// A stream that starts with an ID3v2 tag or a valid frame is passed through untouched.
func mp3Sync(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(10)
	if len(head) >= 3 && string(head[:3]) == "ID3" {
		return br
	}
	win, _ := br.Peek(64 << 10)
	if n, ok := mp3FrameAt(win, 0); ok && mp3NextOK(win, 0, n) {
		return br
	}
	for i := 1; i+4 <= len(win); i++ {
		if n, ok := mp3FrameAt(win, i); ok && mp3NextOK(win, i, n) {
			br.Discard(i)
			return br
		}
	}
	return br // nothing recognisable: let the decoder report it
}

// mp3NextOK reports whether the frame at i of length n is followed by another frame, or the data ends.
func mp3NextOK(b []byte, i, n int) bool {
	j := i + n
	if j+4 > len(b) {
		return true // too little data left in the window to check; accept
	}
	_, ok := mp3FrameAt(b, j)
	return ok
}

// mp3FrameAt parses an MPEG-1/2/2.5 Layer III frame header at b[i:] and returns the frame length.
func mp3FrameAt(b []byte, i int) (int, bool) {
	if i+4 > len(b) || b[i] != 0xFF || b[i+1]&0xE0 != 0xE0 {
		return 0, false
	}
	ver := (b[i+1] >> 3) & 3 // 0 = MPEG 2.5, 2 = MPEG 2, 3 = MPEG 1
	layer := (b[i+1] >> 1) & 3
	if ver == 1 || layer != 1 { // reserved version, or not Layer III
		return 0, false
	}
	bi, si := int(b[i+2]>>4), int(b[i+2]>>2)&3
	if bi == 0 || bi == 15 || si == 3 {
		return 0, false
	}
	pad := int(b[i+2]>>1) & 1
	var kbps int
	var rate int
	if ver == 3 {
		kbps = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bi]
		rate = [...]int{44100, 48000, 32000}[si]
		return 144*kbps*1000/rate + pad, true
	}
	kbps = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bi]
	rate = [...]int{22050, 24000, 16000}[si]
	if ver == 0 {
		rate /= 2
	}
	return 72*kbps*1000/rate + pad, true
}
