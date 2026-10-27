package alac

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Config is the ALACSpecificConfig ("magic cookie") that an MP4 file stores in
// the alac box of its sample description.
type Config struct {
	FrameLength   uint32 // samples per channel in a full frame (4096 normally)
	BitDepth      uint8  // 16 or 24 are decoded
	PB, MB, KB    uint8  // Rice coder parameters (history multiplier, initial history, k modifier)
	Channels      uint8
	MaxRun        uint16
	MaxFrameBytes uint32
	AvgBitrate    uint32
	SampleRate    uint32
}

// ParseConfig reads the 24-byte ALACSpecificConfig.
func ParseConfig(b []byte) (Config, error) {
	if len(b) < 24 {
		return Config{}, errors.New("alac: configuration is shorter than 24 bytes")
	}
	be := binary.BigEndian
	return Config{
		FrameLength: be.Uint32(b), BitDepth: b[5], PB: b[6], MB: b[7], KB: b[8], Channels: b[9],
		MaxRun: be.Uint16(b[10:]), MaxFrameBytes: be.Uint32(b[12:]), AvgBitrate: be.Uint32(b[16:]), SampleRate: be.Uint32(b[20:]),
	}, nil
}

// maxFrameLength bounds what a damaged file can make us allocate. The format's
// own frames are 4096 samples.
const maxFrameLength = 1 << 16

// Decoder decodes the frames of one ALAC track.
type Decoder struct {
	a *Alac
	c Config
}

// NewDecoder returns a decoder for the stream described by c. It refuses what
// it cannot decode (anything but 16 and 24-bit, mono or stereo) so the caller
// can use another decoder instead of getting silence.
func NewDecoder(c Config) (*Decoder, error) {
	switch {
	case c.BitDepth != 16 && c.BitDepth != 24:
		return nil, fmt.Errorf("alac: %d-bit audio is not supported", c.BitDepth)
	case c.Channels < 1 || c.Channels > 2:
		return nil, fmt.Errorf("alac: %d channels are not supported", c.Channels)
	case c.FrameLength == 0 || c.FrameLength > maxFrameLength:
		return nil, fmt.Errorf("alac: frame length %d", c.FrameLength)
	case c.KB > 31 || c.SampleRate == 0: // KB is 0 for files whose frames are all stored uncompressed
		return nil, errors.New("alac: invalid configuration")
	}
	a := create_alac(int(c.BitDepth), int(c.Channels))
	a.setinfo_max_samples_per_frame = c.FrameLength
	a.setinfo_sample_size = c.BitDepth
	a.setinfo_rice_historymult = c.PB
	a.setinfo_rice_initialhistory = c.MB
	a.setinfo_rice_kmodifier = c.KB
	a.setinfo_7f = c.Channels
	a.setinfo_80 = c.MaxRun
	a.setinfo_82 = c.MaxFrameBytes
	a.setinfo_86 = c.AvgBitrate
	a.setinfo_8a_rate = c.SampleRate
	a.allocateBuffers()
	return &Decoder{a: a, c: c}, nil
}

// Config returns the configuration the decoder was made with.
func (d *Decoder) Config() Config { return d.c }

// BytesPerSample is the size of one sample of one channel in Decode's output.
func (d *Decoder) BytesPerSample() int { return int(d.c.BitDepth) / 8 }

// Decode decodes one frame (one MP4 sample) to interleaved little-endian PCM of
// BitDepth bits. The result is only valid until the next call.
func (d *Decoder) Decode(frame []byte) (pcm []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			pcm, err = nil, fmt.Errorf("alac: damaged frame (%v)", r)
		}
	}()
	d.a.err = nil
	out := d.a.decodeFrame(frame)
	if d.a.err != nil {
		return nil, d.a.err
	}
	if out == nil {
		return nil, errors.New("alac: frame produced no audio")
	}
	return out, nil
}

func (alac *Alac) fail(format string, args ...any) {
	if alac.err == nil {
		alac.err = fmt.Errorf("alac: "+format, args...)
	}
}
