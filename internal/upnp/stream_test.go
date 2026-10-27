package upnp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A renderer reads ~80 KB, hangs up and reconnects: the second connection must
// get the whole stream again from the header, and flow-control accounting must
// count each PCM byte once.
func TestStreamReplayAfterProbe(t *testing.T) {
	hdr := WAVHeader(48000)
	st := NewStream(1, WAVFmt{Rate: 48000, Channels: 2, Bits: 16}, hdr)
	pcm := make([]byte, 200_000)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	st.AppendData(pcm)

	var consumed int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.Serve(w, r, ServeHooks{Name: "t", Consumed: func(n int64) { consumed += n }})
	}))
	defer srv.Close()
	want := append(append([]byte(nil), hdr...), pcm...)

	// Probe: read 81920 bytes, then hang up.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	probe := make([]byte, 81920)
	if _, err := io.ReadFull(resp.Body, probe); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(probe, want[:len(probe)]) {
		t.Fatal("probe got the wrong bytes")
	}
	time.Sleep(100 * time.Millisecond)

	// Reconnect: the whole stream again, then a graceful finish ends the response.
	st.Finish(false)
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("replay: %d bytes, err %v (want %d bytes)", len(got), err, len(want))
	}
	if consumed != int64(len(pcm)) {
		t.Errorf("consumed %d, want %d (each PCM byte counted once)", consumed, len(pcm))
	}
	if conns, _, ever := st.ConnState(); conns != 0 || !ever {
		t.Errorf("conn state: %d open, ever=%v", conns, ever)
	}
}

func TestAbortReportsUnserved(t *testing.T) {
	st := NewStream(1, WAVFmt{}, WAVHeader(48000))
	st.AppendData(make([]byte, 1000))
	if n := st.Finish(true); n != 1000 {
		t.Errorf("unserved = %d, want 1000", n)
	}
	if n := st.Finish(true); n != 0 {
		t.Errorf("second abort counted %d again", n)
	}
	if st.AppendData([]byte{1}) {
		t.Error("append after finish succeeded")
	}
}

func TestPacer(t *testing.T) {
	p := NewPacer(1000, 1000) // 1000 B/s, 1000 B burst
	if d := p.Take(1000); d != 0 {
		t.Errorf("burst should be free, waited %v", d)
	}
	d := p.Take(500)
	if d < 450*time.Millisecond || d > 520*time.Millisecond {
		t.Errorf("next 500 bytes should wait ~0.5 s, got %v", d)
	}
	p.Drain()
	if d := p.Take(100); d < 90*time.Millisecond {
		t.Errorf("after Drain the bucket is empty, waited only %v", d)
	}
}
