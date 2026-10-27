package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// pcmDecode decodes PCM audio: WAV and AIFF/AIFC files streamed as-is (the
// header is in the stream), or header-less raw PCM described by the 4 hint
// bytes of the strm command (sample size, rate, channels, endianness).
func pcmDecode(r io.Reader, hint [4]byte, sink Sink) error {
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("PCM stream too short: %w", err)
	}
	var (
		spec pcmSpec
		data io.Reader
		err  error
	)
	switch {
	case string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		spec, err = readWAVHeader(r)
		data = r
	case string(head[0:4]) == "FORM" && (string(head[8:12]) == "AIFF" || string(head[8:12]) == "AIFC"):
		spec, err = readAIFFHeader(r, string(head[8:12]) == "AIFC")
		data = r
	default:
		spec, err = specFromStrm(hint)
		data = io.MultiReader(bytes.NewReader(head), r)
	}
	if err != nil {
		return err
	}
	if spec.limit >= 0 {
		data = io.LimitReader(data, spec.limit)
	}
	return pumpPCM(data, spec, sink)
}

type pcmSpec struct {
	rate, ch, bits int
	bigEndian      bool
	unsigned8      bool  // 8-bit samples are unsigned (WAV, raw) rather than signed (AIFF)
	limit          int64 // audio bytes to read, -1 = until the stream ends
}

func readWAVHeader(r io.Reader) (pcmSpec, error) {
	var sp pcmSpec
	le := binary.LittleEndian
	haveFmt := false
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return sp, fmt.Errorf("WAV: no audio data: %w", err)
		}
		id, size := string(h[0:4]), int64(le.Uint32(h[4:]))
		switch id {
		case "fmt ":
			if size < 16 || size > 1024 {
				return sp, errors.New("WAV: bad fmt chunk")
			}
			b := make([]byte, size+size&1)
			if _, err := io.ReadFull(r, b); err != nil {
				return sp, err
			}
			tag := int(le.Uint16(b[0:]))
			sp.ch = int(le.Uint16(b[2:]))
			sp.rate = int(le.Uint32(b[4:]))
			sp.bits = int(le.Uint16(b[14:]))
			if tag == 0xFFFE && size >= 26 {
				tag = int(le.Uint16(b[24:])) // WAVE_FORMAT_EXTENSIBLE sub-format
			}
			if tag != 1 {
				return sp, fmt.Errorf("WAV: only PCM is supported (format tag %d)", tag)
			}
			sp.unsigned8 = sp.bits == 8
			haveFmt = true
		case "data":
			if !haveFmt {
				return sp, errors.New("WAV: data chunk before fmt chunk")
			}
			sp.limit = -1 // streamed WAVs often carry 0 or 0xFFFFFFFF here
			if size > 0 && size < 0xFFFFFFF0 {
				sp.limit = size
			}
			return sp, nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size&1); err != nil {
				return sp, err
			}
		}
	}
}

func readAIFFHeader(r io.Reader, aifc bool) (pcmSpec, error) {
	sp := pcmSpec{bigEndian: true}
	be := binary.BigEndian
	haveComm := false
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return sp, fmt.Errorf("AIFF: no audio data: %w", err)
		}
		id, size := string(h[0:4]), int64(be.Uint32(h[4:]))
		switch id {
		case "COMM":
			if size < 18 || size > 1024 {
				return sp, errors.New("AIFF: bad COMM chunk")
			}
			b := make([]byte, size+size&1)
			if _, err := io.ReadFull(r, b); err != nil {
				return sp, err
			}
			sp.ch = int(be.Uint16(b[0:]))
			sp.bits = int(be.Uint16(b[6:]))
			// 80-bit extended float sample rate.
			exp := int(be.Uint16(b[8:]) & 0x7FFF)
			mant := be.Uint64(b[10:])
			sp.rate = int(math.Round(math.Ldexp(float64(mant), exp-16383-63)))
			if aifc && size >= 22 {
				switch string(b[18:22]) {
				case "NONE", "twos":
				case "sowt":
					sp.bigEndian = false
				default:
					return sp, fmt.Errorf("AIFC: unsupported compression %q", string(b[18:22]))
				}
			}
			haveComm = true
		case "SSND":
			if !haveComm {
				return sp, errors.New("AIFF: SSND chunk before COMM chunk")
			}
			var o [8]byte // data offset and block size
			if _, err := io.ReadFull(r, o[:]); err != nil {
				return sp, err
			}
			off := int64(be.Uint32(o[0:]))
			if off > 0 {
				if _, err := io.CopyN(io.Discard, r, off); err != nil {
					return sp, err
				}
			}
			sp.limit = -1
			if size >= 8+off && size < 0xFFFFFFF0 && size-8-off > 0 {
				sp.limit = size - 8 - off
			}
			return sp, nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size&1); err != nil {
				return sp, err
			}
		}
	}
}

// specFromStrm decodes the PCM description LMS puts in the strm command when
// the stream has no header: sample size, rate, channels, endianness.
func specFromStrm(f [4]byte) (pcmSpec, error) {
	sp := pcmSpec{limit: -1}
	switch f[0] {
	case '0':
		sp.bits = 8
	case '1':
		sp.bits = 16
	case '2':
		sp.bits = 24
	case '3':
		sp.bits = 32
	default:
		return sp, errors.New("PCM stream has no header and LMS gave no sample size")
	}
	rates := []int{11025, 22050, 32000, 44100, 48000, 8000, 12000, 16000, 24000, 96000}
	if f[1] < '0' || f[1] > '9' {
		return sp, errors.New("PCM stream has no header and LMS gave no sample rate")
	}
	sp.rate = rates[f[1]-'0']
	switch f[2] {
	case '1':
		sp.ch = 1
	case '2':
		sp.ch = 2
	default:
		return sp, errors.New("PCM stream has no header and LMS gave no channel count")
	}
	sp.bigEndian = f[3] == '0'
	sp.unsigned8 = sp.bits == 8
	return sp, nil
}

// pcmSample converts one sample to 16 bits (dropping the low bits of 24/32-bit).
func pcmSample(b []byte, sp pcmSpec) int16 {
	switch sp.bits {
	case 8:
		if sp.unsigned8 {
			return int16(int(b[0])-128) << 8
		}
		return int16(int8(b[0])) << 8
	case 16:
		if sp.bigEndian {
			return int16(binary.BigEndian.Uint16(b))
		}
		return int16(binary.LittleEndian.Uint16(b))
	case 24:
		if sp.bigEndian {
			return int16(uint16(b[0])<<8 | uint16(b[1]))
		}
		return int16(uint16(b[2])<<8 | uint16(b[1]))
	}
	if sp.bigEndian { // 32-bit
		return int16(uint16(b[0])<<8 | uint16(b[1]))
	}
	return int16(uint16(b[3])<<8 | uint16(b[2]))
}

func pumpPCM(r io.Reader, sp pcmSpec, sink Sink) error {
	if sp.ch < 1 || sp.rate <= 0 || (sp.bits != 8 && sp.bits != 16 && sp.bits != 24 && sp.bits != 32) {
		return fmt.Errorf("unsupported PCM format: %d ch, %d Hz, %d bit", sp.ch, sp.rate, sp.bits)
	}
	bps := sp.bits / 8
	frame := sp.ch * bps
	buf := make([]byte, (16384/frame)*frame)
	for {
		n, err := io.ReadFull(r, buf)
		n -= n % frame
		if n > 0 {
			frames := n / frame
			out := make([]int16, frames*2)
			for i := 0; i < frames; i++ {
				off := i * frame
				l := pcmSample(buf[off:], sp)
				rt := l
				if sp.ch > 1 {
					rt = pcmSample(buf[off+bps:], sp)
				}
				out[2*i], out[2*i+1] = l, rt
			}
			if e := sink(out, sp.rate); e != nil {
				return e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
