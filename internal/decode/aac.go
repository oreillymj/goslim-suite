package decode

// Native AAC-LC decoding, using github.com/tphakala/go-aac (pure Go, no cgo).
// It handles ADTS streams and progressive MP4/M4A files, mono or stereo. HE-AAC
// (SBR/PS), LATM, ADIF and ALAC are not covered; with -ffmpeg configured those
// streams are handed to ffmpeg instead.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	aacpcm "github.com/tphakala/go-aac/pcm"
)

var errAACLayout = errors.New("unsupported AAC stream layout")

// aacOut turns the decoder's interleaved S16 output into stereo Sink calls,
// dropping the encoder delay and anything past the real end of the audio.
type aacOut struct {
	sink  Sink
	rate  int
	ch    int
	skip  int64 // frames still to drop
	left  int64 // frames still to play; <0 = no limit
	batch []int16
}

const aacBatch = 4096 // frames per Sink call

func (o *aacOut) full() bool { return o.left == 0 }

// write takes interleaved little-endian S16 with o.ch channels.
func (o *aacOut) write(p []byte) error {
	bytesPer := 2 * o.ch
	n := len(p) / bytesPer
	for i := 0; i < n && o.left != 0; i++ {
		if o.skip > 0 {
			o.skip--
			continue
		}
		l := int16(binary.LittleEndian.Uint16(p[i*bytesPer:]))
		r := l
		if o.ch > 1 {
			r = int16(binary.LittleEndian.Uint16(p[i*bytesPer+2:]))
		}
		o.batch = append(o.batch, l, r)
		if o.left > 0 {
			o.left--
		}
		if len(o.batch) >= 2*aacBatch {
			if err := o.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *aacOut) flush() error {
	if len(o.batch) == 0 {
		return nil
	}
	out := o.batch
	o.batch = make([]int16, 0, 2*aacBatch)
	return o.sink(out, o.rate)
}

// decodeAACNative decodes ADTS or MP4 AAC-LC from r.
func decodeAACNative(r *bufio.Reader, sink Sink) error {
	// Skip an ID3v2 tag in front of an ADTS stream.
	if h, _ := r.Peek(10); len(h) == 10 && string(h[:3]) == "ID3" {
		n := 10 + int(h[6]&0x7f)<<21 | int(h[7]&0x7f)<<14 | int(h[8]&0x7f)<<7 | int(h[9]&0x7f)
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return err
		}
	}
	head, _ := r.Peek(12)
	switch {
	case looksLikeMP4(head):
		return decodeMP4(r, sink)
	case len(head) >= 4 && string(head[:4]) == "ADIF":
		return fmt.Errorf("%w: ADIF", errAACLayout)
	case len(head) >= 2 && head[0] == 0x56 && head[1]&0xe0 == 0xe0:
		return fmt.Errorf("%w: LATM/LOAS", errAACLayout)
	}
	return decodeADTS(r, sink)
}

func decodeADTS(r io.Reader, sink Sink) error {
	d, err := aacpcm.NewDecoder(r)
	if err != nil {
		return err
	}
	info := d.Info()
	if info.Channels < 1 || info.Channels > 2 || info.SampleRate <= 0 {
		return fmt.Errorf("%w: %d channels at %d Hz", errAACLayout, info.Channels, info.SampleRate)
	}
	out := &aacOut{sink: sink, rate: info.SampleRate, ch: info.Channels, left: -1}
	frame := 2 * info.Channels
	buf := make([]byte, 4096*frame)
	for {
		n, err := io.ReadFull(d, buf)
		if n -= n % frame; n > 0 {
			if e := out.write(buf[:n]); e != nil {
				return e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return out.flush()
		}
		if err != nil {
			out.flush() // keep what was decoded before the error
			return err
		}
	}
}

// frameCodec decodes one MP4 sample to interleaved little-endian 16-bit PCM.
type frameCodec interface {
	rate() int
	channels() int
	decode(dst, au []byte) ([]byte, error)
}

type aacCodec struct{ fd *aacpcm.FrameDecoder }

func (c aacCodec) rate() int     { return c.fd.SampleRate() }
func (c aacCodec) channels() int { return c.fd.Channels() }
func (c aacCodec) decode(dst, au []byte) ([]byte, error) {
	out, _, err := c.fd.DecodeFrame(dst[:0], au)
	if errors.Is(err, aacpcm.ErrCorruptStream) {
		err = errSkipUnit
	}
	return out, err
}

// errSkipUnit marks a damaged access unit that is dropped, as a player would.
var errSkipUnit = errors.New("damaged access unit")

// codec makes the decoder for the track's audio.
func (t *mp4Track) newCodec() (frameCodec, error) {
	switch t.codec {
	case "alac":
		return newALACCodec(t.cookie)
	case "aac":
		fd, err := aacpcm.NewRawDecoder(t.asc)
		if err != nil {
			return nil, err
		}
		return aacCodec{fd}, nil
	}
	return nil, fmt.Errorf("%w: %q", errMP4Unsupported, t.codec)
}

// play decodes every access unit of an MP4 track. fetch loads one into buf.
func (t *mp4Track) play(sink Sink, fetch func(mp4Sample, []byte) ([]byte, error)) error {
	fc, err := t.newCodec()
	if err != nil {
		return err
	}
	if ch, rate := fc.channels(), fc.rate(); ch < 1 || ch > 2 || rate <= 0 {
		return fmt.Errorf("%w: %d channels at %d Hz", errAACLayout, ch, rate)
	}
	out := &aacOut{sink: sink, rate: fc.rate(), ch: fc.channels(), skip: t.skip, left: t.total}
	var au, pcm []byte
	for _, s := range t.samples {
		if out.full() {
			break
		}
		if au, err = fetch(s, au); err != nil {
			out.flush()
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return nil // a truncated file plays as far as it goes
			}
			return err
		}
		if pcm, err = fc.decode(pcm, au); err != nil {
			if errors.Is(err, errSkipUnit) {
				continue
			}
			out.flush()
			return err
		}
		if err = out.write(pcm); err != nil {
			return err
		}
	}
	return out.flush()
}

// isUnsupportedAAC reports whether err means "this stream is outside what the
// native decoder does", as opposed to a damaged stream.
func isUnsupportedAAC(err error) bool {
	return errors.Is(err, aacpcm.ErrUnsupported) || errors.Is(err, errMP4Unsupported) || errors.Is(err, errAACLayout)
}

// headRecorder remembers the start of a stream so that it can be replayed into
// another decoder if the first one turns out not to cope.
type headRecorder struct {
	r     io.Reader
	buf   []byte
	limit int
	off   bool // recording stopped (audio has been delivered, or the limit was passed)
	lost  bool // the limit was passed: the start can no longer be replayed
}

func (h *headRecorder) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 && !h.off {
		if len(h.buf)+n > h.limit {
			h.off, h.lost, h.buf = true, true, nil
		} else {
			h.buf = append(h.buf, p[:n]...)
		}
	}
	return n, err
}

func (h *headRecorder) stop() { h.off, h.buf = true, nil }

func (h *headRecorder) replay() (io.Reader, bool) {
	if h.lost || h.off {
		return nil, false
	}
	return io.MultiReader(bytes.NewReader(h.buf), h.r), true
}

// decodeAAC decodes an AAC stream natively, falling back to ffmpeg (if
// configured) for what the native decoder does not handle.
func (d *Decoder) decodeAAC(r io.Reader, sink Sink) error {
	rec := &headRecorder{r: r, limit: 8 << 20}
	delivered := false
	err := decodeAACNative(bufio.NewReaderSize(rec, 64<<10), func(s []int16, rate int) error {
		if !delivered {
			delivered = true
			rec.stop()
		}
		return sink(s, rate)
	})
	if err == nil || !isUnsupportedAAC(err) || delivered {
		return err
	}
	if d.FFmpeg != "" {
		if rest, ok := rec.replay(); ok {
			return d.decodeFFmpeg(rest, sink)
		}
	}
	if d.FFmpeg != "" { // ffmpeg is there, but the start of the stream was too long to replay
		return err
	}
	return fmt.Errorf("%w (not plain AAC-LC: HE-AAC and the like need -ffmpeg, or -nativeaac=false so that LMS transcodes AAC)", err)
}
