package upnpout

import (
	"log"
	"time"
)

var (
	locateEvery = 15 * time.Second // how often a renderer in trouble is checked
	locateAfter = 15 * time.Second // how long it must have been silent before we search for it
)

const locateWatchFor = 30 * time.Minute // how long after a lost stream the renderer is watched

// locate follows a renderer that changes its IP address (DHCP lease renewal): when it has not
// answered for a while, or its stream was lost recently, it is looked up again by MAC and, if it
// has a new address, the output switches to it. Without this a stream lost to an address change
// could never be restarted, because every request still went to the old address.
func (o *Out) locate() {
	if o.rediscover == nil {
		return
	}
	tk := time.NewTicker(locateEvery)
	defer tk.Stop()
	for {
		select {
		case <-o.c.Done():
			return
		case <-tk.C:
		}
		fs := o.failSince.Load()
		recentLoss := time.Since(time.Unix(0, o.streamLostAt.Load())) < locateWatchFor && o.streamLostAt.Load() != 0
		if fs == 0 && !recentLoss {
			continue
		}
		if fs == 0 { // watching after a lost stream: is the renderer answering?
			if _, err := o.up.TransportState(); err == nil {
				continue
			}
			o.failSince.CompareAndSwap(0, time.Now().UnixNano())
			continue // the next round searches if it is still silent
		}
		if time.Since(time.Unix(0, fs)) < locateAfter {
			continue
		}
		cur := o.up.Renderer()
		r := o.rediscover()
		if r == nil || r.IP == nil || cur.IP == nil || r.IP.Equal(cur.IP) {
			continue
		}
		o.up.SetRenderer(r)
		o.failSince.Store(0)
		log.Printf("[%s] the renderer has moved from %s to %s (DHCP?); using the new address", o.name, cur.IP, r.IP)
		if o.onMoved != nil {
			o.onMoved(cur.IP, r.IP)
		}
		o.kickNow()
	}
}
