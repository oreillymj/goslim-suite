package upnp

// ICY (SHOUTcast/Icecast) in-band metadata. A renderer that sends
// "Icy-Metadata: 1" is told "icy-metaint: N" and then gets a metadata block
// after every N bytes of the body: one length byte (in units of 16) followed by
// that many bytes of "StreamTitle='...';StreamUrl='...';", zero padded. The
// title is the track, the URL is the cover art.

import (
	"strings"
	"sync"
	"time"
)

// ICYInterval is the number of body bytes between metadata blocks.
const ICYInterval = 16000

// maxICYBlock is the largest metadata payload the length byte can describe.
const maxICYBlock = 255 * 16

// TrackMeta is what the metadata for one track says. It may be filled in after
// the track was queued, when LMS has answered.
type TrackMeta struct {
	mu         sync.Mutex
	title, art string
	done       chan struct{} // closed by the first Set
}

func NewTrackMeta() *TrackMeta { return &TrackMeta{done: make(chan struct{})} }

// metaWait is how long a connection waits for a track's metadata that LMS has not
// answered yet, when it reaches a metadata block. Without it a renderer that
// fetches the whole track at once would pass every block before the answer.
const metaWait = 1500 * time.Millisecond

// Set fills the metadata in (empty strings if there is none) and releases anyone waiting.
func (m *TrackMeta) Set(title, art string) {
	m.mu.Lock()
	m.title, m.art = title, art
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	m.mu.Unlock()
}

func (m *TrackMeta) get(wait bool) (string, string) {
	if wait {
		select {
		case <-m.done:
		case <-time.After(metaWait):
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.title, m.art
}

type metaMark struct {
	at int64 // PCM offset in the stream where the track starts
	m  *TrackMeta
}

// AddMeta says that the track described by m starts at PCM offset at (without
// the header) of the stream.
func (st *Stream) AddMeta(at int64, m *TrackMeta) {
	st.metaMu.Lock()
	st.marks = append(st.marks, metaMark{at, m})
	if len(st.marks) > 64 {
		st.marks = append([]metaMark(nil), st.marks[len(st.marks)-64:]...)
	}
	st.metaMu.Unlock()
}

// metaAt returns the metadata of the track playing at PCM offset pcm.
func (st *Stream) metaAt(pcm int64) (title, art string) {
	var m *TrackMeta
	st.metaMu.Lock()
	for i := len(st.marks) - 1; i >= 0; i-- {
		if st.marks[i].at <= pcm {
			m = st.marks[i].m
			break
		}
	}
	st.metaMu.Unlock()
	if m == nil {
		return "", ""
	}
	return m.get(true)
}

func icyText(s string) string {
	s = strings.ReplaceAll(s, "'", "’") // the format has no way to escape a quote
	return strings.NewReplacer("\r", " ", "\n", " ", ";", ",").Replace(s)
}

// icyBlock builds one metadata block (length byte included) for a title and
// cover art URL; with neither it is the single zero byte meaning "no change".
func icyBlock(title, art string) []byte {
	if title == "" && art == "" {
		return []byte{0}
	}
	s := "StreamTitle='" + icyText(title) + "';"
	if art != "" {
		s += "StreamUrl='" + icyText(art) + "';"
	}
	b := []byte(s)
	if len(b) > maxICYBlock { // cut the title so that the URL still fits, then anything that is left over
		over := len(b) - maxICYBlock
		t := []byte(icyText(title))
		if over < len(t) {
			return icyBlock(string(t[:len(t)-over-3])+"...", art)
		}
		b = b[:maxICYBlock]
	}
	n := (len(b) + 15) / 16
	out := make([]byte, 1+n*16)
	out[0] = byte(n)
	copy(out[1:], b)
	return out
}

// icyWriter puts metadata into a response body.
type icyWriter struct {
	w     interface{ Write([]byte) (int, error) }
	count int    // body bytes since the last block
	last  string // the title and art last sent, to send a block only on change
	meta  func(pcm int64) (title, art string)
}

// write sends p, which ends at stream offset end (for finding the track), and
// inserts a block each time ICYInterval bytes have gone out.
func (iw *icyWriter) write(p []byte, end int64) error {
	for len(p) > 0 {
		n := ICYInterval - iw.count
		if n > len(p) {
			n = len(p)
		}
		if _, err := iw.w.Write(p[:n]); err != nil {
			return err
		}
		p = p[n:]
		if iw.count += n; iw.count < ICYInterval {
			continue
		}
		iw.count = 0
		title, art := iw.meta(end - int64(len(p)))
		blk := []byte{0}
		if key := title + "\x00" + art; title != "" && key != iw.last {
			iw.last = key
			blk = icyBlock(title, art)
		}
		if _, err := iw.w.Write(blk); err != nil {
			return err
		}
	}
	return nil
}
