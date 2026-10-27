package player

import (
	"encoding/binary"
	"io"
	"time"

	"goslim/internal/audio"
)

// PlayLocal plays this player's audio on the local sound device.
func (c *Client) PlayLocal() (io.Closer, error) {
	return audio.Open(c.outRate, localReader{c})
}

// localReader feeds the sound device: real audio when available, silence
// otherwise, and never blocks.
type localReader struct{ c *Client }

func (l localReader) Read(p []byte) (int, error) {
	c := l.c
	r := c.Pull(p[:len(p)&^3])
	for range r.Starts {
		go c.Announce(time.Now(), time.Now(), c.FetchLater())
	}
	vol := c.Volume()
	if vol < 0.999 {
		for i := 0; i+1 < r.N; i += 2 {
			s := int16(binary.LittleEndian.Uint16(p[i:]))
			binary.LittleEndian.PutUint16(p[i:], uint16(int16(float64(s)*vol)))
		}
	}
	for i := r.N; i < len(p); i++ {
		p[i] = 0
	}
	if r.Emptied {
		c.Notify("STMu")
	}
	return len(p), nil
}
