//go:build !noaudio

// Package audio plays PCM on the local sound device using oto: WASAPI on
// Windows (no cgo), CoreAudio on macOS, ALSA on Linux (needs cgo and the ALSA
// development files to build). Build with -tags noaudio to leave it out.
package audio

import (
	"io"

	"github.com/ebitengine/oto/v3"
)

// Available says whether this build can play on the local sound device.
const Available = true

// Open starts playing r (16-bit stereo little-endian PCM at rate Hz) on the
// default sound device. r should never block: return silence when idle.
func Open(rate int, r io.Reader) (io.Closer, error) {
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   rate,
		ChannelCount: 2,
		Format:       oto.FormatSignedInt16LE,
	})
	if err != nil {
		return nil, err
	}
	<-ready
	p := ctx.NewPlayer(r)
	p.Play()
	return p, nil
}
