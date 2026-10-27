package upnp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestICYBlock(t *testing.T) {
	if b := icyBlock("", ""); !bytes.Equal(b, []byte{0}) {
		t.Errorf("empty block = %v", b)
	}
	b := icyBlock("Artist - It's; a title", "http://h/c.jpg")
	if len(b) != 1+int(b[0])*16 {
		t.Fatalf("length byte %d does not match %d bytes", b[0], len(b))
	}
	body := strings.TrimRight(string(b[1:]), "\x00")
	if body != "StreamTitle='Artist - It’s, a title';StreamUrl='http://h/c.jpg';" {
		t.Errorf("block text %q", body)
	}
	long := icyBlock(strings.Repeat("x", 5000), "http://h/c.jpg")
	if len(long) > 1+maxICYBlock || !strings.Contains(string(long), "StreamUrl='http://h/c.jpg'") {
		t.Errorf("a long title was not shortened to fit (%d bytes)", len(long))
	}
}

// strip removes ICY metadata blocks from a body and returns the audio and the blocks' texts.
func strip(t *testing.T, body []byte, interval int) ([]byte, []string) {
	var audio []byte
	var metas []string
	for len(body) > 0 {
		n := interval
		if n > len(body) {
			n = len(body)
		}
		audio = append(audio, body[:n]...)
		body = body[n:]
		if n < interval || len(body) == 0 {
			break
		}
		l := int(body[0]) * 16
		if len(body) < 1+l {
			t.Fatal("truncated metadata block")
		}
		if l > 0 {
			metas = append(metas, strings.TrimRight(string(body[1:1+l]), "\x00"))
		}
		body = body[1+l:]
	}
	return audio, metas
}

func TestServeICY(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 30000) // 120000 bytes
	hdr := WAVHeader(48000)
	st := NewStream(1, WAVFmt{Rate: 48000, Channels: 2, Bits: 16}, hdr)
	a, b := NewTrackMeta(), NewTrackMeta()
	a.Set("First - One", "http://lms/a.jpg")
	st.AddMeta(0, a)
	b.Set("Second - Two", "")
	st.AddMeta(60000, b)
	st.AppendData(pcm)
	st.Finish(false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.Serve(w, r, ServeHooks{Name: "T", ICY: true})
	}))
	defer srv.Close()

	get := func(icy bool, clen int64) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		if icy {
			req.Header.Set("Icy-Metadata", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, body := get(true, 0)
	if resp.Header.Get("icy-metaint") != strconv.Itoa(ICYInterval) {
		t.Fatalf("icy-metaint = %q", resp.Header.Get("icy-metaint"))
	}
	audio, metas := strip(t, body, ICYInterval)
	if !bytes.Equal(audio, append(append([]byte(nil), hdr...), pcm...)) {
		t.Errorf("the audio changed: %d bytes, want %d", len(audio), len(hdr)+len(pcm))
	}
	if len(metas) != 2 || !strings.HasPrefix(metas[0], "StreamTitle='First - One';StreamUrl='http://lms/a.jpg';") || !strings.HasPrefix(metas[1], "StreamTitle='Second - Two';") {
		t.Errorf("metadata blocks: %q", metas)
	}

	// Without the request header nothing is added.
	resp, body = get(false, 0)
	if resp.Header.Get("icy-metaint") != "" || len(body) != len(hdr)+len(pcm) {
		t.Errorf("plain request got icy-metaint %q and %d bytes", resp.Header.Get("icy-metaint"), len(body))
	}
}
