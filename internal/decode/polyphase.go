package decode

import "math"

// sincKernel is the resampling kernel: a sinc cut off at `cutoff` (relative to
// the input Nyquist frequency) under a 4-term Blackman-Harris window of
// half-width `half` input frames. It is symmetric and zero beyond `half`.
func sincKernel(x, cutoff, half float64) float64 {
	ax := math.Abs(x)
	if ax >= half {
		return 0
	}
	s := 1.0
	if ax != 0 {
		a := math.Pi * cutoff * ax
		s = math.Sin(a) / a
	}
	u := (ax/half + 1) / 2
	w := 0.35875 - 0.48829*math.Cos(2*math.Pi*u) + 0.14128*math.Cos(4*math.Pi*u) - 0.01168*math.Cos(6*math.Pi*u)
	return cutoff * s * w
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// polyphase is the fast path for rates whose ratio is exactly m/l with a
// modest l (44.1 -> 48 kHz is 147/160, 96 -> 48 kHz is 2/1). Every output
// frame then falls on one of l fixed fractional positions, so the filter
// weights for all of them are computed once and the per-frame work is a plain
// multiply-accumulate loop with no function calls.
type polyphase struct {
	m, l int
	hw   int         // frames of context each side (half-width rounded up)
	taps int         // 2*hw
	bank [][]float64 // bank[phase][tap]
	k    int64       // index of the next output frame
	base int64       // absolute index of the first frame in buf
	buf  []int16     // interleaved stereo
}

func newPolyphase(m, l int, step float64) *polyphase {
	cutoff := 0.98 // upsampling: just under the input Nyquist
	if step > 1 {
		cutoff = 0.97 / step // downsampling: just under the output Nyquist
	}
	half := rsHalf / cutoff
	p := &polyphase{m: m, l: l, hw: int(math.Ceil(half))}
	p.taps = 2 * p.hw
	p.bank = make([][]float64, l)
	for ph := range p.bank {
		frac := float64(ph) / float64(l)
		c := make([]float64, p.taps)
		for j := range c {
			c[j] = sincKernel(float64(j-p.hw+1)-frac, cutoff, half)
		}
		p.bank[ph] = c
	}
	p.base = -int64(p.hw - 1)
	p.buf = make([]int16, 2*(p.hw-1)) // silence before the start of the stream
	return p
}

func (p *polyphase) process(in []int16) []int16 {
	p.buf = append(p.buf, in...)
	avail := p.base + int64(len(p.buf)/2) // frames held, absolute
	out := make([]int16, 0, len(in)*p.l/p.m+8)
	for {
		pos := p.k * int64(p.m)
		n, ph := pos/int64(p.l), int(pos%int64(p.l))
		if n+int64(p.hw) >= avail {
			break // not enough input yet for this output frame
		}
		first := int(n - int64(p.hw) + 1 - p.base)
		s := p.buf[2*first : 2*(first+p.taps)]
		var l, r float64
		for j, w := range p.bank[ph] {
			l += w * float64(s[2*j])
			r += w * float64(s[2*j+1])
		}
		out = append(out, Clip16(l), Clip16(r))
		p.k++
	}
	next := p.k * int64(p.m) / int64(p.l)
	if drop := next - int64(p.hw) + 1 - p.base; drop > 0 {
		p.buf = p.buf[2*drop:]
		p.base += drop
	}
	return out
}
