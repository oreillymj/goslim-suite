package decode

// A small MP4/M4A reader: just enough to pull the AAC access units out of a
// progressive file (the usual .m4a), with the encoder-delay trim from its edit
// list. Fragmented files, ALAC and anything else it does not understand return
// errMP4Unsupported so the caller can hand the stream to ffmpeg.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var errMP4Unsupported = errors.New("unsupported MP4 layout")

const (
	maxMoov = 64 << 20  // largest moov box accepted
	maxMdat = 512 << 20 // largest mdat held in memory when the index comes after it

	maxSample = 1 << 20 // largest access unit accepted (a real AAC frame is under 2 KB)
)

// mp4Sample is one access unit's place in the file.
type mp4Sample struct {
	off  int64
	size uint32
}

// mp4Track is what the moov box says about the audio track.
type mp4Track struct {
	codec      string // "aac" or "alac"
	cookie     []byte // ALAC: the 24-byte ALACSpecificConfig
	asc        []byte // AAC: AudioSpecificConfig
	rate       int    // sample rate of the media timescale
	channels   int
	objType    int // MPEG-4 audio object type from the ASC
	avgBitrate int // bits per second from the esds box, 0 if absent
	samples    []mp4Sample
	skip       int64 // PCM frames to drop from the start (encoder delay)
	total      int64 // PCM frames to play after the skip; <0 = to the end
	sorted     bool  // sample offsets increase, so the file can be decoded as it arrives
}

type mp4Box struct {
	typ  string
	data []byte // payload, without the header
}

// mp4Children splits a box payload into the boxes it contains.
func mp4Children(b []byte) []mp4Box {
	var out []mp4Box
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		hdr := 8
		if size == 1 {
			if len(b) < 16 {
				break
			}
			size = binary.BigEndian.Uint64(b[8:])
			hdr = 16
		} else if size == 0 {
			size = uint64(len(b))
		}
		if size < uint64(hdr) || size > uint64(len(b)) {
			break
		}
		out = append(out, mp4Box{typ: string(b[4:8]), data: b[hdr:size]})
		b = b[size:]
	}
	return out
}

func mp4Find(boxes []mp4Box, typ string) *mp4Box {
	for i := range boxes {
		if boxes[i].typ == typ {
			return &boxes[i]
		}
	}
	return nil
}

func mp4Path(b []byte, path ...string) []byte {
	for _, p := range path {
		bx := mp4Find(mp4Children(b), p)
		if bx == nil {
			return nil
		}
		b = bx.data
	}
	return b
}

// parseMoov reads the moov box payload and returns its first audio track.
func parseMoov(moov []byte) (*mp4Track, error) {
	kids := mp4Children(moov)
	if mp4Find(kids, "mvex") != nil {
		return nil, fmt.Errorf("%w: fragmented file", errMP4Unsupported)
	}
	movieScale := uint32(0)
	if mv := mp4Find(kids, "mvhd"); mv != nil && len(mv.data) >= 20 {
		if mv.data[0] == 1 && len(mv.data) >= 28 {
			movieScale = binary.BigEndian.Uint32(mv.data[20:])
		} else {
			movieScale = binary.BigEndian.Uint32(mv.data[12:])
		}
	}
	for _, k := range kids {
		if k.typ != "trak" {
			continue
		}
		t, err := parseTrak(k.data, movieScale)
		if t != nil || err != nil {
			return t, err
		}
	}
	return nil, fmt.Errorf("%w: no audio track", errMP4Unsupported)
}

// parseTrak returns nil, nil for a track that is not audio.
func parseTrak(trak []byte, movieScale uint32) (*mp4Track, error) {
	mdia := mp4Path(trak, "mdia")
	if mdia == nil {
		return nil, nil
	}
	mk := mp4Children(mdia)
	hd := mp4Find(mk, "hdlr")
	if hd == nil || len(hd.data) < 12 || string(hd.data[8:12]) != "soun" {
		return nil, nil
	}
	md := mp4Find(mk, "mdhd")
	if md == nil || len(md.data) < 20 {
		return nil, fmt.Errorf("%w: no mdhd", errMP4Unsupported)
	}
	mediaScale := binary.BigEndian.Uint32(md.data[12:])
	if md.data[0] == 1 && len(md.data) >= 28 {
		mediaScale = binary.BigEndian.Uint32(md.data[20:])
	}
	stbl := mp4Path(mdia, "minf", "stbl")
	if stbl == nil {
		return nil, fmt.Errorf("%w: no sample table", errMP4Unsupported)
	}
	sk := mp4Children(stbl)

	t := &mp4Track{total: -1}
	if err := parseStsd(sk, t); err != nil {
		return nil, err
	}
	if err := parseSamples(sk, t); err != nil {
		return nil, err
	}

	// The edit list says how much encoder delay to drop and how long the
	// audio really is. Only the first real edit is looked at.
	if el := mp4Path(trak, "edts", "elst"); len(el) >= 8 {
		n := binary.BigEndian.Uint32(el[4:])
		v1 := el[0] == 1
		step := 12
		if v1 {
			step = 20
		}
		e := el[8:]
		for i := uint32(0); i < n && len(e) >= step; i, e = i+1, e[step:] {
			var dur uint64
			var mt int64
			if v1 {
				dur, mt = binary.BigEndian.Uint64(e), int64(binary.BigEndian.Uint64(e[8:]))
			} else {
				dur, mt = uint64(binary.BigEndian.Uint32(e)), int64(int32(binary.BigEndian.Uint32(e[4:])))
			}
			if mt < 0 {
				continue // an empty edit: silence at the start
			}
			t.skip = mt
			if movieScale > 0 && mediaScale > 0 && dur > 0 {
				t.total = int64(dur * uint64(mediaScale) / uint64(movieScale))
			}
			break
		}
	}
	if mediaScale > 0 && t.rate == 0 {
		t.rate = int(mediaScale)
	}
	return t, nil
}

func parseStsd(stbl []mp4Box, t *mp4Track) error {
	sd := mp4Find(stbl, "stsd")
	if sd == nil || len(sd.data) < 16 {
		return fmt.Errorf("%w: no stsd", errMP4Unsupported)
	}
	entry := mp4Children(sd.data[8:])
	if len(entry) == 0 {
		return fmt.Errorf("%w: empty stsd", errMP4Unsupported)
	}
	e := entry[0]
	if e.typ != "mp4a" && e.typ != "alac" {
		return fmt.Errorf("%w: %q audio (not AAC or ALAC)", errMP4Unsupported, e.typ)
	}
	d := e.data
	if len(d) < 28 {
		return fmt.Errorf("%w: short mp4a entry", errMP4Unsupported)
	}
	ver := binary.BigEndian.Uint16(d[8:])
	t.channels = int(binary.BigEndian.Uint16(d[16:]))
	t.rate = int(binary.BigEndian.Uint32(d[24:]) >> 16)
	skip := 28
	switch ver {
	case 1:
		skip += 16
	case 2:
		skip += 36
	}
	if len(d) < skip {
		return fmt.Errorf("%w: short mp4a entry", errMP4Unsupported)
	}
	if e.typ == "alac" {
		t.codec = "alac"
		box := mp4Find(mp4Children(d[skip:]), "alac")
		if box == nil || len(box.data) < 28 { // version and flags, then the 24-byte configuration
			return fmt.Errorf("%w: no ALAC configuration", errMP4Unsupported)
		}
		t.cookie = append([]byte(nil), box.data[4:28]...)
		t.channels = int(t.cookie[9])
		t.rate = int(binary.BigEndian.Uint32(t.cookie[20:])) // the 16.16 field above cannot hold 96 kHz
		return nil
	}
	t.codec = "aac"
	es := mp4Find(mp4Children(d[skip:]), "esds")
	if es == nil || len(es.data) < 4 {
		return fmt.Errorf("%w: no esds", errMP4Unsupported)
	}
	return parseESDS(es.data[4:], t)
}

// mdLen reads an MPEG-4 descriptor length (1 to 4 bytes, 7 bits each).
func mdLen(b []byte) (n int, used int, ok bool) {
	for i := 0; i < 4 && i < len(b); i++ {
		n = n<<7 | int(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return n, i + 1, true
		}
	}
	return 0, 0, false
}

func parseESDS(b []byte, t *mp4Track) error {
	for len(b) >= 2 {
		tag := b[0]
		n, used, ok := mdLen(b[1:])
		if !ok {
			break
		}
		body := b[1+used:]
		if n > len(body) {
			n = len(body)
		}
		body = body[:n]
		switch tag {
		case 0x03: // ES_Descriptor: ES_ID, flags, optional fields, then sub-descriptors
			if len(body) < 3 {
				return fmt.Errorf("%w: short ES descriptor", errMP4Unsupported)
			}
			fl := body[2]
			p := 3
			if fl&0x80 != 0 {
				p += 2
			}
			if fl&0x40 != 0 && p < len(body) {
				p += 1 + int(body[p])
			}
			if fl&0x20 != 0 {
				p += 2
			}
			if p > len(body) {
				return fmt.Errorf("%w: bad ES descriptor", errMP4Unsupported)
			}
			return parseESDS(body[p:], t)
		case 0x04: // DecoderConfigDescriptor
			if len(body) < 13 {
				return fmt.Errorf("%w: short decoder config", errMP4Unsupported)
			}
			if oti := body[0]; oti != 0x40 && oti != 0x66 && oti != 0x67 && oti != 0x68 {
				return fmt.Errorf("%w: object type 0x%02x is not AAC", errMP4Unsupported, oti)
			}
			t.avgBitrate = int(binary.BigEndian.Uint32(body[9:]))
			return parseESDS(body[13:], t)
		case 0x05: // DecoderSpecificInfo: the AudioSpecificConfig
			t.asc = append([]byte(nil), body...)
			return nil
		}
		b = b[1+used+n:]
	}
	if t.asc == nil {
		return fmt.Errorf("%w: no AudioSpecificConfig", errMP4Unsupported)
	}
	return nil
}

// parseSamples builds the table of access units from stsz, stsc and stco/co64.
func parseSamples(stbl []mp4Box, t *mp4Track) error {
	sz, sc := mp4Find(stbl, "stsz"), mp4Find(stbl, "stsc")
	co := mp4Find(stbl, "stco")
	wide := false
	if co == nil {
		co, wide = mp4Find(stbl, "co64"), true
	}
	if sz == nil || sc == nil || co == nil || len(sz.data) < 12 || len(sc.data) < 8 || len(co.data) < 8 {
		return fmt.Errorf("%w: incomplete sample table", errMP4Unsupported)
	}
	fixed := binary.BigEndian.Uint32(sz.data[4:])
	count := int(binary.BigEndian.Uint32(sz.data[8:]))
	if fixed == 0 && len(sz.data) < 12+4*count {
		return fmt.Errorf("%w: truncated stsz", errMP4Unsupported)
	}
	if count < 0 || count > 1<<22 {
		return fmt.Errorf("%w: absurd sample count", errMP4Unsupported)
	}
	size := func(i int) uint32 {
		if fixed != 0 {
			return fixed
		}
		return binary.BigEndian.Uint32(sz.data[12+4*i:])
	}

	nch := int(binary.BigEndian.Uint32(co.data[4:]))
	cw := 4
	if wide {
		cw = 8
	}
	if len(co.data) < 8+cw*nch {
		return fmt.Errorf("%w: truncated chunk offsets", errMP4Unsupported)
	}
	chunk := func(i int) int64 {
		if wide {
			return int64(binary.BigEndian.Uint64(co.data[8+8*i:]))
		}
		return int64(binary.BigEndian.Uint32(co.data[8+4*i:]))
	}

	nsc := int(binary.BigEndian.Uint32(sc.data[4:]))
	if len(sc.data) < 8+12*nsc || nsc == 0 {
		return fmt.Errorf("%w: truncated stsc", errMP4Unsupported)
	}
	t.samples = make([]mp4Sample, 0, count)
	t.sorted = true
	idx, last := 0, int64(-1)
	for e := 0; e < nsc; e++ {
		first := int(binary.BigEndian.Uint32(sc.data[8+12*e:]))
		per := int(binary.BigEndian.Uint32(sc.data[12+12*e:]))
		end := nch + 1
		if e+1 < nsc {
			end = int(binary.BigEndian.Uint32(sc.data[8+12*(e+1):]))
		}
		for c := first; c < end && c <= nch; c++ {
			off := chunk(c - 1)
			for s := 0; s < per && idx < count; s++ {
				n := size(idx)
				if off < last {
					t.sorted = false
				}
				t.samples = append(t.samples, mp4Sample{off: off, size: n})
				off += int64(n)
				last = off
				idx++
			}
		}
	}
	if idx != count {
		return fmt.Errorf("%w: sample table does not add up", errMP4Unsupported)
	}
	return nil
}

// ------------------------------------------------------------------ reading

// looksLikeMP4 reports whether b begins with an MP4 box header.
func looksLikeMP4(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	switch string(b[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "styp", "pnot", "uuid":
		return true
	}
	return false
}

// mp4HeadLen says how much of the start of a file to look at so that its moov
// box (if it comes first) is included, up to 1 MiB.
func mp4HeadLen(head []byte) int {
	pos := 0
	for pos+8 <= len(head) {
		size := int64(binary.BigEndian.Uint32(head[pos:]))
		typ := string(head[pos+4 : pos+8])
		if size == 1 && pos+16 <= len(head) {
			size = int64(binary.BigEndian.Uint64(head[pos+8:]))
		}
		if size < 8 {
			break
		}
		if typ == "moov" {
			if end := int64(pos) + size; end <= 1<<20 {
				return int(end)
			}
			break
		}
		if typ == "mdat" {
			break
		}
		pos += int(size)
	}
	return 16 << 10
}

// mp4Meta finds the audio track described by the moov box inside head, if there
// is one.
func mp4Meta(head []byte) *mp4Track {
	if b := mp4Find(mp4Children(head), "moov"); b != nil {
		if t, err := parseMoov(b.data); err == nil {
			return t
		}
	}
	return nil
}

// boxReader walks the top-level boxes of a stream.
type boxReader struct {
	r   *bufio.Reader
	pos int64
}

func (b *boxReader) discard(n int64) error {
	m, err := io.CopyN(io.Discard, b.r, n)
	b.pos += m
	return err
}

// next reads a box header. size is -1 for a box that runs to the end of the
// stream.
func (b *boxReader) next() (typ string, size int64, err error) {
	var h [16]byte
	if _, err = io.ReadFull(b.r, h[:8]); err != nil {
		return "", 0, err
	}
	b.pos += 8
	size = int64(binary.BigEndian.Uint32(h[:]))
	typ = string(h[4:8])
	switch size {
	case 0:
		return typ, -1, nil
	case 1:
		if _, err = io.ReadFull(b.r, h[8:16]); err != nil {
			return "", 0, err
		}
		b.pos += 8
		size = int64(binary.BigEndian.Uint64(h[8:]))
		return typ, size - 16, nil
	}
	return typ, size - 8, nil
}

// decodeMP4 decodes the AAC in an MP4 stream. If the index (moov) arrives
// before the audio (mdat) the audio is decoded as it arrives; if it comes after,
// the audio is held in memory until the index is known.
func decodeMP4(br *bufio.Reader, sink Sink) error {
	bx := &boxReader{r: br}
	var track *mp4Track
	var held []byte // mdat payload read before the index was seen
	var heldOff int64

	for {
		typ, size, err := bx.next()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		switch typ {
		case "moov":
			if size < 0 || size > maxMoov {
				return fmt.Errorf("%w: moov size %d", errMP4Unsupported, size)
			}
			buf, err := readExactly(br, size) // grows as data arrives: a bogus size costs nothing
			if err != nil {
				return fmt.Errorf("reading moov: %w", err)
			}
			bx.pos += size
			if track, err = parseMoov(buf); err != nil {
				return err
			}
			if held != nil {
				return playMP4(track, bytes.NewReader(held), heldOff, sink)
			}
		case "moof", "sidx":
			return fmt.Errorf("%w: fragmented file", errMP4Unsupported)
		case "mdat":
			if track != nil && track.sorted {
				return playMP4Stream(track, bx, size, sink)
			}
			if size < 0 || size > maxMdat {
				return fmt.Errorf("%w: mdat size %d", errMP4Unsupported, size)
			}
			heldOff = bx.pos
			if held, err = readExactly(br, size); err != nil {
				return fmt.Errorf("reading mdat: %w", err)
			}
			bx.pos += size
			if track != nil {
				return playMP4(track, bytes.NewReader(held), heldOff, sink)
			}
		default:
			if size < 0 {
				return fmt.Errorf("%w: open-ended %q box", errMP4Unsupported, typ)
			}
			if err := bx.discard(size); err != nil {
				return err
			}
		}
	}
	if track == nil {
		return fmt.Errorf("%w: no moov box", errMP4Unsupported)
	}
	return errors.New("MP4 has no audio data")
}

// playMP4 decodes samples held in memory; data[0] is at file offset base.
func playMP4(t *mp4Track, data *bytes.Reader, base int64, sink Sink) error {
	size := data.Size()
	return t.play(sink, func(s mp4Sample, buf []byte) ([]byte, error) {
		o := s.off - base
		if s.size > maxSample || o < 0 || o+int64(s.size) > size {
			return nil, fmt.Errorf("sample at offset %d lies outside the audio data", s.off)
		}
		buf = grow(buf, int(s.size))
		_, err := data.ReadAt(buf, o)
		return buf, err
	})
}

// playMP4Stream decodes samples straight off the stream, in file order.
func playMP4Stream(t *mp4Track, bx *boxReader, size int64, sink Sink) error {
	return t.play(sink, func(s mp4Sample, buf []byte) ([]byte, error) {
		if s.off < bx.pos {
			return nil, fmt.Errorf("sample at offset %d is behind the read position", s.off)
		}
		if err := bx.discard(s.off - bx.pos); err != nil {
			return nil, err
		}
		if s.size > maxSample {
			return nil, fmt.Errorf("sample of %d bytes", s.size)
		}
		buf = grow(buf, int(s.size))
		n, err := io.ReadFull(bx.r, buf)
		bx.pos += int64(n)
		return buf, err
	})
}

// readExactly reads n bytes, allocating only as they arrive.
func readExactly(r io.Reader, n int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err == nil && int64(len(b)) != n {
		err = io.ErrUnexpectedEOF
	}
	return b, err
}

func grow(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}
