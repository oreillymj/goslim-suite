package decode

import (
	"encoding/binary"
	"fmt"
)

var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

var aacObjects = map[int]string{1: "Main", 2: "LC", 3: "SSR", 4: "LTP", 5: "HE (SBR)", 29: "HEv2 (SBR+PS)", 42: "xHE"}

// probeAAC describes an ADTS or MP4 AAC stream from its first bytes.
func probeAAC(b []byte) (Info, bool) {
	if len(b) >= 10 && string(b[:3]) == "ID3" { // skip a tag in front of ADTS
		n := 10 + int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		if n < len(b) {
			b = b[n:]
		}
	}
	if looksLikeMP4(b) {
		t := mp4Meta(b)
		if t == nil {
			return Info{Codec: "AAC", Detail: "in MP4 (index at the end of the file)"}, true
		}
		return describeAAC(t.objType, t.asc, t.rate, t.channels, t.avgBitrate, "MP4"), true
	}
	for i := 0; i+7 <= len(b) && i < 4096; i++ {
		if b[i] != 0xff || b[i+1]&0xf6 != 0xf0 {
			continue
		}
		profile := int(b[i+2]>>6) + 1
		idx := int(b[i+2] >> 2 & 0xf)
		ch := int(b[i+2]&1)<<2 | int(b[i+3]>>6)
		if idx >= len(aacRates) {
			continue
		}
		info := Info{Codec: "AAC", Rate: aacRates[idx], Channels: ch}
		info.Detail = fmt.Sprintf("%s, ADTS", aacObjects[profile])
		return info, true
	}
	return Info{Codec: "AAC"}, false
}

func describeAAC(obj int, asc []byte, rate, ch, bitrate int, container string) Info {
	if len(asc) >= 2 { // object type (5 bits), rate index (4), channel config (4)
		obj = int(asc[0] >> 3)
		if idx := int(asc[0]&7)<<1 | int(asc[1]>>7); idx < len(aacRates) {
			rate = aacRates[idx]
		}
		ch = int(asc[1] >> 3 & 0xf)
	}
	detail := aacObjects[obj]
	if detail == "" {
		detail = fmt.Sprintf("AAC object type %d", obj)
	}
	detail += " in " + container
	if bitrate > 0 {
		detail += fmt.Sprintf(", %d kbps", (bitrate+500)/1000)
	}
	return Info{Codec: "AAC", Detail: detail, Rate: rate, Channels: ch}
}

// probeALAC describes an ALAC MP4 stream from its first bytes.
func probeALAC(b []byte) (Info, bool) {
	if !looksLikeMP4(b) {
		return Info{Codec: "ALAC"}, false
	}
	t := mp4Meta(b)
	if t == nil {
		return Info{Codec: "ALAC", Detail: "in MP4 (index at the end of the file)"}, true
	}
	info := Info{Codec: "ALAC", Detail: "in MP4", Rate: t.rate, Channels: t.channels}
	if t.codec == "alac" && len(t.cookie) >= 24 {
		info.Bits = int(t.cookie[5])
	}
	return info, true
}

// probeVorbis reads the identification header of an Ogg Vorbis stream.
func probeVorbis(b []byte) (Info, bool) {
	if len(b) < 28 || string(b[:4]) != "OggS" {
		return Info{Codec: "Ogg"}, false
	}
	p := b[27+int(b[26]):] // first packet of the first page
	if len(p) >= 28 && p[0] == 1 && string(p[1:7]) == "vorbis" {
		info := Info{Codec: "Vorbis", Channels: int(p[11]), Rate: int(binary.LittleEndian.Uint32(p[12:]))}
		if nominal := int32(binary.LittleEndian.Uint32(p[20:])); nominal > 0 {
			info.Detail = fmt.Sprintf("in Ogg, about %d kbps", (int(nominal)+500)/1000)
		} else {
			info.Detail = "in Ogg"
		}
		return info, true
	}
	return Info{Codec: "Ogg", Detail: "not Vorbis"}, true
}
