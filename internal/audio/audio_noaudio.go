//go:build noaudio

// Package audio is the stub used by builds made with -tags noaudio.
package audio

import (
	"errors"
	"io"
)

// Available says whether this build can play on the local sound device.
const Available = false

// Open always fails: this build has no local audio output.
func Open(rate int, r io.Reader) (io.Closer, error) {
	return nil, errors.New("this build has no local audio output (built with -tags noaudio); use UPnP renderers instead")
}
