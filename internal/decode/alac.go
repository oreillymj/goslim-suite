package decode

// Native ALAC (Apple Lossless) decoding from MP4/M4A, using the vendored
// decoder in internal/alac. 16 and 24-bit, mono and stereo; anything else is
// handed to ffmpeg when one is configured.

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"goslim/internal/alac"
)

type alacCodec struct {
	d    *alac.Decoder
	conf alac.Config
	buf  []byte
}

func newALACCodec(cookie []byte) (frameCodec, error) {
	conf, err := alac.ParseConfig(cookie)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMP4Unsupported, err)
	}
	d, err := alac.NewDecoder(conf)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMP4Unsupported, err)
	}
	return &alacCodec{d: d, conf: conf}, nil
}

func (c *alacCodec) rate() int     { return int(c.conf.SampleRate) }
func (c *alacCodec) channels() int { return int(c.conf.Channels) }

// decode returns 16-bit samples; 24-bit audio keeps its top 16 bits, as for every format.
func (c *alacCodec) decode(_, au []byte) ([]byte, error) {
	pcm, err := c.d.Decode(au)
	if err != nil {
		return nil, errSkipUnit
	}
	if c.d.BytesPerSample() == 2 {
		return pcm, nil
	}
	n := len(pcm) / 3
	if cap(c.buf) < 2*n {
		c.buf = make([]byte, 2*n)
	}
	c.buf = c.buf[:2*n]
	for i := 0; i < n; i++ {
		c.buf[2*i], c.buf[2*i+1] = pcm[3*i+1], pcm[3*i+2]
	}
	return c.buf, nil
}

// decodeALAC decodes an ALAC MP4 stream natively, falling back to ffmpeg for
// what the native decoder does not handle.
func (d *Decoder) decodeALAC(r io.Reader, sink Sink) error {
	rec := &headRecorder{r: r, limit: 8 << 20}
	delivered := false
	br := bufio.NewReaderSize(rec, 64<<10)
	var err error
	if head, _ := br.Peek(12); !looksLikeMP4(head) {
		err = fmt.Errorf("%w: not an MP4 file", errMP4Unsupported)
	} else {
		err = decodeMP4(br, func(s []int16, rate int) error {
			if !delivered {
				delivered = true
				rec.stop()
			}
			return sink(s, rate)
		})
	}
	if err == nil || !errors.Is(err, errMP4Unsupported) || delivered {
		return err
	}
	if d.FFmpeg != "" {
		if rest, ok := rec.replay(); ok {
			return d.decodeFFmpeg(rest, sink)
		}
		return err
	}
	return fmt.Errorf("%w (this ALAC needs -ffmpeg, or -nativealac=false so that LMS transcodes it)", err)
}
