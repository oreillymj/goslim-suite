package upnpout

import (
	"log"
	"time"

	"goslim/internal/decode"
	"goslim/internal/player"
	"goslim/internal/upnp"
)

// pumpRaw is passthrough mode: LMS's original stream is served to the renderer
// untouched, one URL per track, so the renderer does the decoding. Tracks play
// one after another; the next is not taken from the player until the renderer
// has finished the last, so LMS's idea of the current track follows the audio.
// There is a short gap between tracks, ReplayGain is not applied, and the
// elapsed time is estimated from the wall clock.
func (o *Out) pumpRaw() {
	c := o.c
	buf := make([]byte, 64<<10)
	gen := o.flushGen.Load()
	var waitFor *upnp.Stream // the previous track, which the renderer must finish before the next is taken
	var lastCheck time.Time
	for {
		select {
		case <-c.Done():
			return
		default:
		}
		if g := o.flushGen.Load(); g != gen {
			gen, waitFor = g, nil
		}
		if o.gone.Swap(false) {
			o.handleGone()
		}
		if waitFor != nil {
			if time.Since(lastCheck) >= time.Second && o.rawFinished(waitFor) {
				waitFor = nil
				if o.pendingEnd.Swap(false) {
					log.Printf("[%s] playback finished", o.name)
					c.Notify("STMu")
				}
			} else {
				lastCheck = lastCheckTime(lastCheck, waitFor, o)
				o.wait()
			}
			continue
		}

		g0 := o.flushGen.Load()
		r := c.Pull(buf)
		if r.N > 0 {
			if !o.rawAppend(buf[:r.N], r, g0, gen) {
				continue
			}
		}
		if r.Ended > 0 {
			o.mu.Lock()
			st := o.cur
			o.mu.Unlock()
			if st != nil {
				st.Finish(false) // the whole track is in: the response ends when the renderer has it
				waitFor = st
			}
		}
		if r.Emptied {
			o.pendingEnd.Store(true)
		}
		if r.N == 0 && r.Ended == 0 && !r.Emptied {
			o.wait()
		}
	}
}

// lastCheckTime remembers when the renderer was last asked, but only when it
// really was (rawFinished queries it once the stream has been fully served).
func lastCheckTime(prev time.Time, st *upnp.Stream, o *Out) time.Time {
	if conns, _, _ := st.ConnState(); conns == 0 && st.Drained() {
		return time.Now()
	}
	return prev
}

// rawFinished reports whether the renderer has played out the stream: it has
// been sent everything, has hung up, and has stopped.
func (o *Out) rawFinished(st *upnp.Stream) bool {
	o.mu.Lock()
	cur := o.cur
	o.mu.Unlock()
	if cur != st { // replaced or flushed
		return true
	}
	conns, _, _ := st.ConnState()
	if conns != 0 || !st.Drained() {
		return false
	}
	ts, err := o.up.TransportState()
	return err == nil && (ts == "STOPPED" || ts == "NO_MEDIA_PRESENT")
}

// rawAppend adds a track's bytes to its stream, starting the stream (and the
// renderer) when the track's first bytes arrive.
func (o *Out) rawAppend(p []byte, r player.PullResult, g0, gen int64) bool {
	o.mu.Lock()
	need := r.Started || o.cur == nil
	o.mu.Unlock()
	if need && o.recover.Load() && !o.waitReachable(gen) {
		return false
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.flushGen.Load() != g0 {
		return false
	}
	o.mu.Lock()
	st := o.cur
	o.lastData = time.Now()
	o.mu.Unlock()
	if need || st == nil {
		mime := decode.ContentType(r.Format, p)
		if r.Started {
			o.tracks.Add(1)
			o.logResumes()
		}
		st = o.begin(0, mime, extFor(mime))
		o.clockReset()
	}
	o.sent.Add(int64(len(p)))
	at := st.Appended()
	if !st.AppendData(p) {
		o.consumed.Add(int64(len(p)))
	}
	if r.Started {
		o.markTrack(st, at, o.c.FetchLater())
	}
	return true
}

func extFor(mime string) string {
	switch mime {
	case "audio/mpeg":
		return "mp3"
	case "audio/flac":
		return "flac"
	case "audio/aiff":
		return "aiff"
	case "audio/aac":
		return "aac"
	case "audio/mp4":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	}
	return "wav"
}
