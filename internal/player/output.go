package player

import "time"

// Output receives a player's decoded audio when it isn't played on the local
// sound device. The Client calls these when LMS issues commands; the audio
// itself is pulled from the Client by the output's own pump (see Client.Pull).
type Output interface {
	Flushed()                       // LMS stopped or flushed the queue
	Pause()                         // LMS paused
	Resume()                        // LMS resumed
	SetVolume(gain float64)         // LMS volume change; gain is linear, 1.0 = full scale
	SetName(name string)            // LMS renamed the player
	Inflight() int64                // PCM bytes handed over but not yet played (for elapsed time)
	Elapsed() (time.Duration, bool) // how far into the current track the output really is, if it knows
	Close()                         // the player is shutting down: stop the renderer, release resources
}
