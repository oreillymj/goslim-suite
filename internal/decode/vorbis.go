package decode

// Ogg Vorbis decoding, using github.com/jfreymuth/oggvorbis (pure Go, MIT).

import (
	"fmt"
	"io"

	"github.com/jfreymuth/oggvorbis"
)

// vorbisPair picks the left and right channels out of the interleaved frame.
// Vorbis orders 3, 5, 6, 7 and 8 channels as front left, centre, front right...
// and 4 channels as front left, front right, rear left, rear right.
func vorbisPair(ch int) (l, r int) {
	switch ch {
	case 3, 5, 6, 7, 8:
		return 0, 2
	case 1:
		return 0, 0
	}
	return 0, 1
}

func f32to16(v float32) int16 {
	v *= 32768
	switch {
	case v >= 32767:
		return 32767
	case v <= -32768:
		return -32768
	case v >= 0:
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}

func decodeVorbis(r io.Reader, sink Sink) (err error) {
	defer func() { // a damaged stream must not take the player down
		if p := recover(); p != nil {
			err = fmt.Errorf("vorbis: damaged stream (%v)", p)
		}
	}()
	// Hide Seek: with a seekable source the reader jumps to the end to measure the file.
	vr, err := oggvorbis.NewReader(struct{ io.Reader }{r})
	if err != nil {
		return fmt.Errorf("vorbis: %w", err)
	}
	ch, rate := vr.Channels(), vr.SampleRate()
	if ch < 1 || rate <= 0 {
		return fmt.Errorf("vorbis: %d channels at %d Hz", ch, rate)
	}
	li, ri := vorbisPair(ch)
	buf := make([]float32, 4096*ch)
	for {
		n, rerr := vr.Read(buf)
		if frames := n / ch; frames > 0 {
			out := make([]int16, 2*frames)
			for i := 0; i < frames; i++ {
				out[2*i], out[2*i+1] = f32to16(buf[i*ch+li]), f32to16(buf[i*ch+ri])
			}
			if e := sink(out, rate); e != nil {
				return e
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("vorbis: %w", rerr)
		}
	}
}
