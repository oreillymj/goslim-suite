package upnpout

import (
	"log"
	"time"

	"goslim/internal/upnp"
)

// watchdog ends a stream when LMS stops sending, so the renderer plays out its
// buffer and stops instead of waiting forever.
func (o *Out) watchdog() {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-o.c.Done():
			return
		case <-tk.C:
		}
		o.mu.Lock()
		st := o.cur
		idle := st != nil && !o.paused && st.Drained() && time.Since(o.lastData) > o.idle
		if idle {
			o.cur = nil
		}
		o.mu.Unlock()
		if idle {
			log.Printf("[%s] no audio for %s, ending stream", o.name, o.idle)
			st.Finish(false)
		}
	}
}

// poll watches the renderer's transport state. It logs changes and mirrors a
// pause made from the renderer's side (e.g. the Sonos app) into LMS, and the
// matching resume. It ignores the few seconds after our own commands, and
// needs consecutive polls to agree, so transitions don't cause false alarms. A
// renderer that reports STOPPED while we are playing, and yet keeps the stream
// open, is treated like one that dropped it.
func (o *Out) poll() {
	c := o.c
	last, mism, stopped := "", 0, 0
	var lastStats time.Time
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-c.Done():
			return
		case <-tk.C:
		}
		o.mu.Lock()
		st, paused, ext := o.cur, o.paused, o.extPaused
		recent := time.Since(o.cmdAt) < 4*time.Second
		o.mu.Unlock()
		if st == nil {
			last, mism, stopped = "", 0, 0
			continue
		}
		ts, err := o.up.TransportState()
		if err != nil {
			if upnp.Verbose {
				log.Printf("[%s] GetTransportInfo: %v", o.name, err)
			}
			continue
		}
		o.mu.Lock()
		o.lastState = ts
		o.mu.Unlock()
		if ts != last {
			log.Printf("[%s] renderer transport state: %s", o.name, ts)
			last = ts
		}
		if upnp.Verbose && time.Since(lastStats) >= time.Minute {
			lastStats = time.Now()
			log.Printf("[%s] stream %d: %s sent, %s served, transport state %s", o.name, st.ID,
				upnp.HumanBytes(st.Appended()), upnp.HumanBytes(st.Served()), ts)
		}
		if recent {
			mism, stopped = 0, 0
			continue
		}
		switch {
		case ts == "PAUSED_PLAYBACK" && !paused && !ext:
			if mism++; mism >= 2 {
				mism = 0
				o.mu.Lock()
				o.paused, o.extPaused = true, true
				o.mu.Unlock()
				if st.Pace != nil {
					st.Pace.Drain()
				}
				o.clockPause()
				c.PauseFromOutput()
				log.Printf("[%s] paused from the renderer side; pausing LMS", o.name)
			}
		case ts == "PLAYING" && ext:
			if mism++; mism >= 2 {
				mism = 0
				o.mu.Lock()
				o.paused, o.extPaused = false, false
				o.lastData = time.Now()
				o.mu.Unlock()
				o.clockResume()
				c.ResumeFromOutput()
				log.Printf("[%s] resumed from the renderer side; resuming LMS", o.name)
			}
		case ts == "STOPPED" && !o.raw && !paused:
			mism = 0
			if stopped++; stopped >= 3 {
				stopped = 0
				log.Printf("[%s] the renderer reports STOPPED but has not ended stream %d; treating the stream as lost", o.name, st.ID)
				o.endStream(st, "renderer reports STOPPED but kept the stream open")
			}
		default:
			mism, stopped = 0, 0
		}
	}
}
