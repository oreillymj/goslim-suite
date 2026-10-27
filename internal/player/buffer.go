package player

import (
	"errors"
	"sync"
)

const maxBuffered = 8 << 20 // decoded PCM buffered per track (~43 s of 48 kHz stereo)

var errClosed = errors.New("buffer closed")

// pcmBuf holds one track's decoded PCM between the decoder and the output.
type pcmBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	done   bool // decoder finished writing
	closed bool // cancelled (flush)
}

func newPCMBuf() *pcmBuf {
	b := &pcmBuf{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pcmBuf) write(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for !b.closed && len(b.data) >= maxBuffered {
		b.cond.Wait()
	}
	if b.closed {
		return errClosed
	}
	b.data = append(b.data, p...)
	return nil
}

// read is non-blocking and returns whatever is available.
func (b *pcmBuf) read(p []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := copy(p, b.data)
	b.data = b.data[n:]
	if n > 0 {
		b.cond.Broadcast()
	}
	return n
}

func (b *pcmBuf) len() int       { b.mu.Lock(); defer b.mu.Unlock(); return len(b.data) }
func (b *pcmBuf) finished() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.done && len(b.data) == 0 }
func (b *pcmBuf) isClosed() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.closed }
func (b *pcmBuf) finish()        { b.mu.Lock(); b.done = true; b.mu.Unlock() }
func (b *pcmBuf) close()         { b.mu.Lock(); b.closed = true; b.cond.Broadcast(); b.mu.Unlock() }
