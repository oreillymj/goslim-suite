package upnpout

import (
	"fmt"
	"log"
	"time"

	"goslim/internal/upnp"
)

// pumpPCM drains the player's track queue into the renderer's WAV stream,
// never getting more than o.window ahead of what the renderer has pulled. It
// sleeps whenever there is nothing to do and is woken by the player (more
// audio) or by the renderer (progress), so an idle player uses no CPU.
func (o *Out) pumpPCM() {
	c := o.c
	buf := make([]byte, feedChunk)
	var marks []trackMark
	gen := o.flushGen.Load()
	for {
		select {
		case <-c.Done():
			return
		default:
		}
		if g := o.flushGen.Load(); g != gen {
			gen, marks = g, nil // flushed: those tracks will never be heard
		}
		marks = o.service(marks)
		if o.Inflight() >= o.window {
			o.wait()
			continue
		}

		g0 := o.flushGen.Load()
		r := c.Pull(buf)
		base := o.sent.Load()
		if r.N > 0 {
			rate := r.Rate
			if rate == 0 {
				rate = c.OutRate()
			}
			if !o.feedPCM(buf[:r.N], rate, r.Starts, g0) {
				continue // flushed while we were pulling
			}
		}
		for _, off := range r.Starts {
			info := c.FetchLater()
			if o.icy { // the bytes from the track's start to the end of this chunk are all in the current stream
				o.mu.Lock()
				st := o.cur
				o.mu.Unlock()
				if st != nil {
					info = o.markTrack(st, st.Appended()-int64(r.N-off), info)
				}
			}
			marks = append(marks, trackMark{at: base + int64(off), queued: time.Now(), info: info})
		}
		if r.Emptied {
			o.pendingEnd.Store(true)
		}
		if r.N == 0 && r.Ended == 0 && !r.Emptied {
			o.wait()
		}
	}
}

// service logs track transitions the renderer has reached, reports the end of
// playback, and reacts to the renderer's stream disappearing.
func (o *Out) service(marks []trackMark) []trackMark {
	// A track is audible once the renderer has pulled its first byte.
	if len(marks) > 0 && o.consumed.Load() > marks[0].at {
		m := marks[0]
		marks = marks[1:]
		o.tracks.Add(1)
		o.logResumes()
		go o.c.Announce(time.Now(), m.queued, m.info)
	}
	// Report "track finished" only once the renderer has pulled everything.
	if o.pendingEnd.Load() && o.consumed.Load() >= o.sent.Load() {
		o.pendingEnd.Store(false)
		log.Printf("[%s] playback finished", o.name)
		o.c.Notify("STMu")
	}
	if o.gone.Swap(false) {
		o.handleGone()
	}
	return marks
}

// feedPCM hands PCM to the renderer's stream. When the stream has to be
// replaced first - the sample rate changed, or it is nearing the 4 GiB limit of
// a WAV file - the old one is finished and the renderer given time to play it
// out before the new one starts. It reports false if the queue was flushed.
func (o *Out) feedPCM(p []byte, rate int, starts []int, gen int64) bool {
	o.mu.Lock()
	st := o.cur
	o.mu.Unlock()
	cut, reason := -1, "" // offset in p where a new stream must begin
	if st != nil {
		app := st.Appended()
		switch {
		case int(st.Fmt.Rate) != rate:
			cut, reason = 0, fmt.Sprintf("the sample rate changes from %g to %g kHz", float64(st.Fmt.Rate)/1000, float64(rate)/1000)
		case app >= o.rotateBytes+rotateSlack:
			cut, reason = 0, "the stream has become too long for a WAV file"
		case app >= o.rotateBytes && len(starts) > 0:
			cut, reason = starts[0], "the stream is nearing the 4 GiB limit of a WAV file"
		}
	}
	if cut > 0 {
		if !o.appendPCM(p[:cut], int(st.Fmt.Rate), gen) {
			return false
		}
		p = p[cut:]
	}
	if cut >= 0 && !o.handOver(reason, gen) {
		return false
	}
	return o.appendPCM(p, rate, gen)
}

// appendPCM adds PCM to the current stream, starting one if there is none.
func (o *Out) appendPCM(p []byte, rate int, gen int64) bool {
	if len(p) == 0 {
		return true
	}
	o.mu.Lock()
	need := o.cur == nil
	o.mu.Unlock()
	if need && o.recover.Load() && !o.waitReachable(gen) {
		return false
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.flushGen.Load() != gen {
		return false
	}
	o.mu.Lock()
	st := o.cur
	o.lastData = time.Now()
	o.mu.Unlock()
	if st == nil {
		st = o.begin(rate, "", "wav")
	}
	o.sent.Add(int64(len(p)))
	if !st.AppendData(p) {
		o.consumed.Add(int64(len(p))) // the stream was closed under us: nothing will play this
	}
	return true
}

// handOver finishes the current stream and waits until the renderer has played
// it out and stopped, so the next stream doesn't cut off the audio the renderer
// still has buffered (a Sonos can hold more than a minute).
func (o *Out) handOver(reason string, gen int64) bool {
	o.mu.Lock()
	st := o.cur
	o.mu.Unlock()
	if st == nil {
		return true
	}
	log.Printf("[%s] starting a new stream because %s; letting the renderer play out stream %d first (%s sent)",
		o.name, reason, st.ID, upnp.HumanBytes(st.Appended()))
	st.Finish(false) // graceful: the renderer gets every byte, then the response ends
	limit := 100 * time.Second
	if o.bufferSecs > 0 {
		limit = time.Duration(o.bufferSecs*float64(time.Second)) + 20*time.Second
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !o.sleep(500*time.Millisecond) || o.flushGen.Load() != gen {
			return false
		}
		if conns, _, _ := st.ConnState(); conns == 0 && st.Drained() {
			if ts, err := o.up.TransportState(); err == nil && (ts == "STOPPED" || ts == "NO_MEDIA_PRESENT") {
				break
			}
		}
	}
	o.mu.Lock()
	if o.cur == st {
		o.cur = nil
	}
	o.mu.Unlock()
	return true
}
