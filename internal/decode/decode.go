// Package decode turns the audio formats LMS can send into 16-bit stereo PCM.
//
// Supported natively: MP3, FLAC, PCM (WAV, AIFF/AIFC or raw), AAC-LC (ADTS or
// MP4/M4A, mono or stereo), ALAC (16 and 24-bit, mono or stereo) and Ogg
// Vorbis. HE-AAC and the ALAC the native decoder cannot handle go to an external
// ffmpeg when one is configured; otherwise LMS is left to transcode such files.
package decode

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/hajimehoshi/go-mp3"
	"github.com/mewkiz/flac"
)

// Sink receives decoded audio: interleaved stereo 16-bit samples at rate Hz.
type Sink func(samples []int16, rate int) error

// Format letters, as used by the Slimproto strm command.
const (
	FormatMP3  = 'm'
	FormatFLAC = 'f'
	FormatPCM  = 'p'
	FormatAAC  = 'a'
	FormatALAC = 'l'
	FormatOgg  = 'o' // Ogg Vorbis
)

// Decoder decodes the formats this build supports.
type Decoder struct {
	FFmpeg  string // path to ffmpeg; enables ALAC and the AAC that is not plain LC
	OutRate int    // sample rate ffmpeg is asked to produce

	// NoNativeAAC switches the built-in AAC-LC decoder off: AAC is then decoded
	// by ffmpeg if there is one, and otherwise not offered to LMS (which
	// transcodes AAC files itself).
	NoNativeAAC bool

	// NoNativeALAC and NoNativeVorbis do the same for ALAC (then ffmpeg or LMS
	// transcoding) and Ogg Vorbis (then LMS transcodes it, ffmpeg is not used).
	NoNativeALAC   bool
	NoNativeVorbis bool
}

// FindFFmpeg resolves an ffmpeg executable name or path.
func FindFFmpeg(p string) (string, error) { return exec.LookPath(p) }

// Caps is the format list offered to LMS in the HELO message.
func (d *Decoder) Caps() string {
	caps := "flc,pcm,mp3"
	if d.Supported(FormatAAC) {
		caps += ",aac"
	}
	if d.Supported(FormatALAC) {
		caps += ",alc"
	}
	if d.Supported(FormatOgg) {
		caps += ",ogg"
	}
	return caps
}

// Supported reports whether format can be decoded.
func (d *Decoder) Supported(format byte) bool {
	switch format {
	case FormatMP3, FormatFLAC, FormatPCM:
		return true
	case FormatAAC:
		return !d.NoNativeAAC || d.FFmpeg != ""
	case FormatALAC:
		return !d.NoNativeALAC || d.FFmpeg != ""
	case FormatOgg:
		return !d.NoNativeVorbis
	}
	return false
}

// Decode reads the whole stream r and passes the audio to sink. pcmHint holds
// the sample size, rate, channels and endianness characters of the strm
// command and is only used for header-less PCM.
func (d *Decoder) Decode(format byte, r io.Reader, pcmHint [4]byte, sink Sink) error {
	return d.DecodeInfo(format, r, pcmHint, sink, nil)
}

// DecodeInfo is Decode that also reports what the stream is (from its first
// bytes) to onInfo before any audio is decoded.
func (d *Decoder) DecodeInfo(format byte, r io.Reader, pcmHint [4]byte, sink Sink, onInfo func(Info)) error {
	if onInfo != nil {
		br := bufio.NewReaderSize(r, 1<<20) // big enough to see past a large ID3 tag
		head, _ := br.Peek(10)
		want := 16 << 10
		if len(head) == 10 && string(head[:3]) == "ID3" {
			want = 10 + int(head[6]&0x7f)<<21 | int(head[7]&0x7f)<<14 | int(head[8]&0x7f)<<7 | int(head[9]&0x7f) + 8<<10
		}
		if want > 1<<20 {
			want = 1 << 20
		}
		head, _ = br.Peek(want)
		if (format == FormatAAC || format == FormatALAC) && looksLikeMP4(head) {
			if n := mp4HeadLen(head); n > len(head) || n > want { // the moov box can be big
				head, _ = br.Peek(n)
			}
		}
		if info, ok := Probe(format, head, pcmHint); ok {
			onInfo(info)
		}
		r = br
	}
	switch format {
	case FormatMP3:
		return decodeMP3(r, sink)
	case FormatFLAC:
		return decodeFLAC(r, sink)
	case FormatPCM:
		return pcmDecode(r, pcmHint, sink)
	case FormatAAC:
		if d.NoNativeAAC {
			return d.decodeFFmpeg(r, sink)
		}
		return d.decodeAAC(r, sink)
	case FormatALAC:
		if d.NoNativeALAC {
			return d.decodeFFmpeg(r, sink)
		}
		return d.decodeALAC(r, sink)
	case FormatOgg:
		return decodeVorbis(r, sink)
	}
	return fmt.Errorf("unsupported format %q", format)
}

func decodeMP3(r io.Reader, sink Sink) error {
	d, err := mp3.NewDecoder(r)
	if err != nil {
		return err
	}
	buf := make([]byte, 16384)
	for {
		n, err := io.ReadFull(d, buf)
		n -= n % 4
		if n > 0 {
			samples := make([]int16, n/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			if e := sink(samples, d.SampleRate()); e != nil {
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

func to16(v int32, bps int) int16 {
	switch {
	case bps > 16:
		return int16(v >> (bps - 16))
	case bps < 16:
		return int16(v << (16 - bps))
	}
	return int16(v)
}

func decodeFLAC(r io.Reader, sink Sink) error {
	st, err := flac.New(r)
	if err != nil {
		return err
	}
	bps, rate := int(st.Info.BitsPerSample), int(st.Info.SampleRate)
	for {
		fr, err := st.ParseNext()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		n := int(fr.BlockSize)
		out := make([]int16, n*2)
		for i := 0; i < n; i++ {
			l := fr.Subframes[0].Samples[i]
			rt := l
			if len(fr.Subframes) > 1 {
				rt = fr.Subframes[1].Samples[i]
			}
			out[2*i], out[2*i+1] = to16(l, bps), to16(rt, bps)
		}
		if e := sink(out, rate); e != nil {
			return e
		}
	}
}

// decodeFFmpeg pipes the stream through ffmpeg, which writes 16-bit stereo PCM
// at OutRate. MP4 files whose index (moov atom) sits at the end can't be read
// from a pipe; ffmpeg's complaint is then returned as the error.
func (d *Decoder) decodeFFmpeg(r io.Reader, sink Sink) error {
	cmd := exec.Command(d.FFmpeg, "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-vn",
		"-f", "s16le", "-ac", "2", "-ar", strconv.Itoa(d.OutRate), "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	go func() {
		io.Copy(stdin, r) // ends when the LMS stream ends or is flushed
		stdin.Close()
	}()

	buf := make([]byte, 16384)
	got := false
	for {
		n, err := io.ReadFull(stdout, buf)
		n -= n % 4
		if n > 0 {
			got = true
			samples := make([]int16, n/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			if e := sink(samples, d.OutRate); e != nil {
				return e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if !got {
				cmd.Wait() // so stderr is complete before we read it
				return fmt.Errorf("ffmpeg produced no audio: %s", strings.TrimSpace(stderr.String()))
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}
