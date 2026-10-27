package upnpout

import (
	"fmt"
	"log"
	"strings"
	"time"

	"goslim/internal/upnp"
)

// maxRestarts is how many stream losses are kept for the summary at shutdown.
const maxRestarts = 100

// restart describes one unexpected loss of the renderer's stream: what was
// going on at that moment, so that a pattern (a time of day, a stream age or
// size, a transport state) can be looked for afterwards.
type restart struct {
	n         int // 1-based, counts every loss since start
	at        time.Time
	reason    string
	stream    int
	age       time.Duration // how long the stream had been running
	appended  int64         // bytes handed to this stream
	served    int64         // bytes the renderer pulled from it
	sentTotal int64         // cumulative bytes handed to all streams since start
	consTotal int64         // cumulative bytes consumed since start
	inflight  int64
	state     string        // last transport state seen by the poller
	sinceReq  time.Duration // since the renderer last pulled audio; <0 = never
	track     int           // tracks started since the program began
	paused    bool
	resumed   bool          // autoresume was on, so a new stream was (or will be) started
	recovered time.Duration // how long until the renderer answered again (0 = not yet / not applicable)
}

// recordRestart notes a lost stream. It is called with the stream that ended.
func (o *Out) recordRestart(st *upnp.Stream, reason string) {
	o.mu.Lock()
	r := restart{
		at: time.Now(), reason: reason, stream: st.ID, appended: st.Appended(), served: st.Served(),
		sentTotal: o.sent.Load(), consTotal: o.consumed.Load(), state: o.lastState, track: int(o.tracks.Load()),
		paused: o.paused, resumed: o.autoResume, age: time.Since(o.streamBegan), sinceReq: -1,
	}
	if t := o.lastPull.Load(); t != 0 {
		r.sinceReq = time.Since(time.Unix(0, t))
	}
	o.lost++
	r.n = o.lost
	r.inflight = r.sentTotal - r.consTotal
	if r.resumed {
		o.resumes++
	}
	o.history = append(o.history, r)
	if len(o.history) > maxRestarts {
		o.history = o.history[len(o.history)-maxRestarts:]
	}
	o.lostAt = r.at
	o.mu.Unlock()
}

// noteRecovered stores how long the renderer took to answer after the last loss.
func (o *Out) noteRecovered() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n := len(o.history); n > 0 && o.history[n-1].resumed && o.history[n-1].recovered == 0 && !o.lostAt.IsZero() {
		o.history[n-1].recovered = max(time.Since(o.lostAt).Round(time.Millisecond), time.Millisecond)
	}
}

// Resumes is how many times a lost stream has been restarted by autoresume.
func (o *Out) Resumes() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resumes
}

// logResumes is the verbose line between tracks.
func (o *Out) logResumes() {
	if !upnp.Verbose {
		return
	}
	o.mu.Lock()
	n, lost, at := o.resumes, o.lost, o.lostAt
	o.mu.Unlock()
	last := ""
	if lost > 0 {
		last = fmt.Sprintf(", last lost stream at %s", at.Format("2006-01-02 15:04:05"))
	}
	log.Printf("[%s] autoresumes so far: %d (streams lost: %d%s)", o.name, n, lost, last)
}

// logRestartSummary prints what was kept about lost streams. It runs when the
// player is closed (Ctrl-C), once.
func (o *Out) logRestartSummary() {
	o.mu.Lock()
	h := append([]restart(nil), o.history...)
	lost, resumes := o.lost, o.resumes
	o.mu.Unlock()
	if lost == 0 {
		if upnp.Verbose {
			log.Printf("[%s] restart summary: the renderer's stream was never lost", o.name)
		}
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] restart summary: %d streams lost, %d autoresumes; the last %d are listed, oldest first\n", o.name, lost, resumes, len(h))
	for _, r := range h {
		rec := "-"
		if r.recovered > 0 {
			rec = r.recovered.String()
		}
		since := "-"
		if r.sinceReq >= 0 {
			since = r.sinceReq.Round(time.Second).String()
		}
		fmt.Fprintf(&b, "  #%d %s stream %d: %s | stream age %s, %s appended, %s served | total %s sent, %s consumed, %s in flight | state %s, last data pulled %s ago, track %d, paused %v | autoresume %v, recovered after %s\n",
			r.n, r.at.Format("2006-01-02 15:04:05"), r.stream, r.reason, r.age.Round(time.Second),
			upnp.HumanBytes(r.appended), upnp.HumanBytes(r.served), upnp.HumanBytes(r.sentTotal), upnp.HumanBytes(r.consTotal),
			upnp.HumanBytes(r.inflight), orDash(r.state), since, r.track, r.paused, r.resumed, rec)
	}
	log.Print(strings.TrimRight(b.String(), "\n"))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
