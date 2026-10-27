package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

// Info describes an input stream: what LMS is sending, before any decoding.
type Info struct {
	Codec    string // MP3, FLAC, WAV, AIFF, PCM, AAC/ALAC
	Detail   string // e.g. "MPEG-1 Layer III, 320 kbps CBR"
	Rate     int    // Hz
	Channels int
	Mode     string // MP3 channel mode (stereo, joint stereo, ...); replaces the plain channel word
	Bits     int    // 0 for lossy formats
}

func (i Info) String() string {
	var parts []string
	if i.Detail != "" {
		parts = append(parts, i.Detail)
	}
	if i.Bits > 0 {
		parts = append(parts, fmt.Sprintf("%d-bit", i.Bits))
	}
	if i.Rate > 0 {
		parts = append(parts, fmt.Sprintf("%g kHz", float64(i.Rate)/1000))
	}
	switch {
	case i.Mode != "":
		parts = append(parts, i.Mode)
	case i.Channels == 0:
	case i.Channels == 1:
		parts = append(parts, "mono")
	case i.Channels == 2:
		parts = append(parts, "stereo")
	default:
		parts = append(parts, fmt.Sprintf("%d channels", i.Channels))
	}
	return strings.TrimSpace(i.Codec + " " + strings.Join(parts, ", "))
}

// Probe reads what it can about a stream from its first bytes. hint is the
// strm command's PCM description, used for header-less PCM.
func Probe(format byte, head []byte, hint [4]byte) (Info, bool) {
	switch format {
	case FormatMP3:
		return probeMP3(head)
	case FormatFLAC:
		return probeFLAC(head)
	case FormatPCM:
		return probePCM(head, hint)
	case FormatAAC:
		return probeAAC(head)
	case FormatALAC:
		return probeALAC(head)
	case FormatOgg:
		return probeVorbis(head)
	}
	return Info{}, false
}

// ---------------------------------------------------------------- FLAC

func probeFLAC(b []byte) (Info, bool) {
	if len(b) < 26 || string(b[:4]) != "fLaC" || b[4]&0x7f != 0 { // first block must be STREAMINFO
		return Info{}, false
	}
	v := binary.BigEndian.Uint64(b[18:26])
	info := Info{Codec: "FLAC", Rate: int(v >> 44), Channels: int((v>>41)&7) + 1, Bits: int((v>>36)&31) + 1}
	if total := v & (1<<36 - 1); total > 0 && info.Rate > 0 {
		info.Detail = fmt.Sprintf("lossless, %d:%02d", int(total)/info.Rate/60, int(total)/info.Rate%60)
	} else {
		info.Detail = "lossless"
	}
	return info, true
}

// ---------------------------------------------------------------- PCM

func probePCM(b []byte, hint [4]byte) (Info, bool) {
	if len(b) >= 12 {
		switch {
		case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
			if sp, err := readWAVHeader(bytes.NewReader(b[12:])); err == nil {
				return Info{Codec: "WAV", Detail: "PCM", Rate: sp.rate, Channels: sp.ch, Bits: sp.bits}, true
			}
			return Info{Codec: "WAV"}, true
		case string(b[0:4]) == "FORM" && (string(b[8:12]) == "AIFF" || string(b[8:12]) == "AIFC"):
			if sp, err := readAIFFHeader(bytes.NewReader(b[12:]), string(b[8:12]) == "AIFC"); err == nil {
				return Info{Codec: "AIFF", Detail: "PCM", Rate: sp.rate, Channels: sp.ch, Bits: sp.bits}, true
			}
			return Info{Codec: "AIFF"}, true
		}
	}
	if sp, err := specFromStrm(hint); err == nil {
		return Info{Codec: "PCM", Detail: "raw", Rate: sp.rate, Channels: sp.ch, Bits: sp.bits}, true
	}
	return Info{Codec: "PCM"}, true
}

// ---------------------------------------------------------------- MP3

type mp3Header struct {
	version  int // 1, 2 (MPEG-2) or 25 (MPEG-2.5)
	layer    int // 1, 2 or 3
	crc      bool
	bitrate  int // kbps, 0 = free format
	rate     int
	padding  int
	mode     int // 0 stereo, 1 joint stereo, 2 dual channel, 3 mono
	frameLen int // bytes, layer III only
}

var (
	mp3BitratesV1L3 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1}
	mp3BitratesV2L3 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1}
	mp3Rates        = map[int][3]int{1: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 25: {11025, 12000, 8000}}
	mp3Modes        = [4]string{"stereo", "joint stereo", "dual channel", "mono"}
)

func parseMP3Header(b []byte) (mp3Header, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return mp3Header{}, false
	}
	var h mp3Header
	switch (b[1] >> 3) & 3 {
	case 3:
		h.version = 1
	case 2:
		h.version = 2
	case 0:
		h.version = 25
	default:
		return h, false
	}
	switch (b[1] >> 1) & 3 {
	case 3:
		h.layer = 1
	case 2:
		h.layer = 2
	case 1:
		h.layer = 3
	default:
		return h, false
	}
	h.crc = b[1]&1 == 0
	br, sr := int(b[2]>>4), int((b[2]>>2)&3)
	if br == 15 || sr == 3 {
		return h, false
	}
	h.rate = mp3Rates[h.version][sr]
	h.padding = int((b[2] >> 1) & 1)
	h.mode = int(b[3] >> 6)
	if h.layer == 3 {
		if h.version == 1 {
			h.bitrate = mp3BitratesV1L3[br]
			h.frameLen = 144000*h.bitrate/h.rate + h.padding
		} else {
			h.bitrate = mp3BitratesV2L3[br]
			h.frameLen = 72000*h.bitrate/h.rate + h.padding
		}
	}
	return h, true
}

func (h mp3Header) samplesPerFrame() int {
	if h.version == 1 {
		return 1152
	}
	return 576
}

func probeMP3(b []byte) (Info, bool) {
	i := 0
	if len(b) >= 10 && string(b[:3]) == "ID3" { // skip the ID3v2 tag
		i = 10 + int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		if b[5]&0x10 != 0 {
			i += 10
		}
	}
	for ; i+4 <= len(b); i++ {
		h, ok := parseMP3Header(b[i : i+4])
		if !ok {
			continue
		}
		// Confirm with the frame after it, so a stray 0xFFE in the data doesn't fool us.
		if h.layer == 3 && h.frameLen > 4 {
			if i+h.frameLen+4 <= len(b) {
				if _, ok := parseMP3Header(b[i+h.frameLen : i+h.frameLen+4]); !ok {
					continue
				}
			}
		}
		return describeMP3(b, i, h), true
	}
	return Info{Codec: "MP3"}, false
}

func describeMP3(b []byte, at int, h mp3Header) Info {
	name := map[int]string{1: "MPEG-1", 2: "MPEG-2", 25: "MPEG-2.5"}[h.version]
	info := Info{Codec: "MP3", Rate: h.rate, Channels: 2}
	if h.mode == 3 {
		info.Channels = 1
	}
	if h.layer != 3 {
		info.Detail = fmt.Sprintf("%s Layer %d", name, h.layer)
		return info
	}
	info.Mode = mp3Modes[h.mode]
	detail := fmt.Sprintf("%s Layer III", name)
	// A Xing/Info header sits after the side information of the first frame.
	side := 32
	switch {
	case h.version == 1 && h.mode == 3:
		side = 17
	case h.version != 1 && h.mode != 3:
		side = 17
	case h.version != 1:
		side = 9
	}
	off := at + 4 + side
	if h.crc {
		off += 2
	}
	kind := ""
	if off+8 <= len(b) {
		switch string(b[off : off+4]) {
		case "Xing":
			kind = "VBR"
		case "Info":
			kind = "CBR"
		}
	}
	if kind == "" && at+36+4 <= len(b) && string(b[at+36:at+40]) == "VBRI" {
		kind = "VBR"
	}
	if kind != "" && off+8 <= len(b) && (string(b[off:off+4]) == "Xing" || string(b[off:off+4]) == "Info") {
		flags := binary.BigEndian.Uint32(b[off+4:])
		p := off + 8
		var frames, bytesN uint32
		if flags&1 != 0 && p+4 <= len(b) {
			frames = binary.BigEndian.Uint32(b[p:])
			p += 4
		}
		if flags&2 != 0 && p+4 <= len(b) {
			bytesN = binary.BigEndian.Uint32(b[p:])
		}
		if kind == "VBR" && frames > 0 && bytesN > 0 {
			secs := float64(frames) * float64(h.samplesPerFrame()) / float64(h.rate)
			info.Detail = fmt.Sprintf("%s, VBR avg %d kbps", detail, int(float64(bytesN)*8/secs/1000+0.5))
			return info
		}
		if kind == "CBR" {
			info.Detail = fmt.Sprintf("%s, %d kbps CBR", detail, h.bitrate)
			return info
		}
		info.Detail = detail + ", VBR"
		return info
	}
	if kind == "VBR" {
		info.Detail = detail + ", VBR"
		return info
	}
	// No tag: compare the bitrates of the next few frames.
	rates := map[int]bool{h.bitrate: true}
	pos := at + h.frameLen
	for n := 0; n < 12 && pos > at && pos+4 <= len(b); n++ {
		next, ok := parseMP3Header(b[pos : pos+4])
		if !ok || next.layer != 3 || next.frameLen <= 4 {
			break
		}
		rates[next.bitrate] = true
		pos += next.frameLen
	}
	if len(rates) > 1 {
		info.Detail = detail + ", VBR"
	} else {
		info.Detail = fmt.Sprintf("%s, %d kbps CBR", detail, h.bitrate)
	}
	return info
}

// ContentType is the MIME type to serve an undecoded stream as.
func ContentType(format byte, head []byte) string {
	switch format {
	case FormatMP3:
		return "audio/mpeg"
	case FormatFLAC:
		return "audio/flac"
	case FormatPCM:
		if len(head) >= 4 && string(head[:4]) == "FORM" {
			return "audio/aiff"
		}
		return "audio/wav"
	case FormatAAC:
		if looksLikeMP4(head) {
			return "audio/mp4"
		}
		return "audio/aac"
	case FormatALAC:
		return "audio/mp4"
	case FormatOgg:
		return "audio/ogg"
	}
	return "application/octet-stream"
}

// RawPCMHeader builds a streaming WAV header for header-less PCM described by
// the strm command's hint, so it can be passed on as a WAV file. It fails for
// big-endian PCM, which WAV cannot hold.
func RawPCMHeader(hint [4]byte) ([]byte, bool) {
	sp, err := specFromStrm(hint)
	if err != nil || sp.bigEndian || sp.ch < 1 || sp.ch > 2 {
		return nil, false
	}
	h := make([]byte, 44)
	le := binary.LittleEndian
	copy(h[0:], "RIFF")
	le.PutUint32(h[4:], 0xFFFFFFFF)
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], uint16(sp.ch))
	le.PutUint32(h[24:], uint32(sp.rate))
	le.PutUint32(h[28:], uint32(sp.rate*sp.ch*sp.bits/8))
	le.PutUint16(h[32:], uint16(sp.ch*sp.bits/8))
	le.PutUint16(h[34:], uint16(sp.bits))
	copy(h[36:], "data")
	le.PutUint32(h[40:], 0xFFFFFFFF)
	return h, true
}
