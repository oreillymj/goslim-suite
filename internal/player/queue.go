package player

// PullResult describes what Pull did.
type PullResult struct {
	N       int   // bytes copied
	Started bool  // a track began during this call
	Ended   int   // tracks fully drained
	Emptied bool  // the queue ran dry (playback finished)
	Starts  []int // byte offsets in p where tracks began

	Rate   int  // sample rate of the PCM in p; 0 if nothing was copied (raw bytes carry no rate)
	Raw    bool // p holds the undecoded bytes of one track (passthrough mode)
	Format byte // format letter of that raw track
}

// Pull copies decoded PCM from the queued tracks into p (len(p) must be a
// multiple of 4). It never blocks and returns nothing while paused or stopped.
// The first time a track's audio is handed out, LMS is told the track started
// (STMs).
//
// A call never mixes sample rates (they only differ when resampling is off),
// and in passthrough mode never mixes tracks: it returns the original bytes of
// one track at a time.
func (c *Client) Pull(p []byte) (r PullResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.playing {
		return
	}
	var first *track
	for r.N < len(p) && len(c.queue) > 0 {
		t := c.queue[0]
		if t.raw {
			if first != nil && t != first {
				break
			}
			r.Raw, r.Format = true, t.format
		} else {
			rate := int(t.rate.Load())
			if rate == 0 && !t.buf.finished() {
				break // the decoder hasn't produced anything yet
			}
			if rate != 0 {
				if r.N > 0 && rate != r.Rate {
					break // never mix sample rates in one chunk
				}
				r.Rate = rate
			}
		}
		first = t
		if !t.started {
			t.started = true
			r.Started = true
			r.Starts = append(r.Starts, r.N)
			go c.stat("STMs", 0)
		}
		n := t.buf.read(p[r.N:])
		r.N += n
		t.played += int64(n)
		if n == 0 {
			if t.buf.finished() {
				c.queue = c.queue[1:]
				r.Ended++
				if len(c.queue) == 0 {
					c.playing = false
					r.Emptied = true
				}
				continue
			}
			break // underrun: wait for the decoder
		}
	}
	return
}
