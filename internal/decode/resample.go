package decode

import "math"

const (
	rsHalf = 16  // sinc zero crossings on each side of the centre (at the cutoff frequency)
	rsRes  = 256 // kernel table entries per input frame
)

// Resampler converts interleaved stereo int16 audio from one sample rate to
// another with a windowed-sinc (4-term Blackman-Harris) kernel, which keeps
// the passband flat and rejects aliasing when going down. step is input
// frames per output frame (inRate/outRate). It is streaming: feed it
// consecutive chunks and it carries the history it needs between calls.
type Resampler struct {
	step float64
	pos  float64 // centre of the next output frame, in input frames from the start of buf
	buf  []int16 // interleaved input still needed (history plus not yet consumed)
	tab  []float64
	half float64 // kernel half-width in input frames

	poly *polyphase // fast path for exact rational ratios; nil = the general path below
}

// NewResampler converts from inRate to outRate.
func NewResampler(inRate, outRate int) *Resampler {
	r := &Resampler{step: float64(inRate) / float64(outRate)}
	g := gcd(inRate, outRate)
	if l := outRate / g; l <= 4096 {
		r.poly = newPolyphase(inRate/g, l, r.step)
	}
	return r
}

func (r *Resampler) init() {
	cutoff := 0.98 // upsampling: just under the input Nyquist
	if r.step > 1 {
		cutoff = 0.97 / r.step // downsampling: just under the output Nyquist
	}
	r.half = rsHalf / cutoff
	r.tab = make([]float64, int(r.half*rsRes)+2)
	for k := range r.tab {
		x := float64(k) / rsRes
		if x >= r.half {
			continue
		}
		sinc := 1.0
		if x != 0 {
			a := math.Pi * cutoff * x
			sinc = math.Sin(a) / a
		}
		u := (x/r.half + 1) / 2
		w := 0.35875 - 0.48829*math.Cos(2*math.Pi*u) + 0.14128*math.Cos(4*math.Pi*u) - 0.01168*math.Cos(6*math.Pi*u)
		r.tab[k] = cutoff * sinc * w
	}
}

func (r *Resampler) kernel(ax float64) float64 {
	if ax >= r.half {
		return 0
	}
	v := ax * rsRes
	i := int(v)
	f := v - float64(i)
	return r.tab[i]*(1-f) + r.tab[i+1]*f
}

func Clip16(v float64) int16 {
	v = math.Round(v)
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func (r *Resampler) Process(in []int16) []int16 {
	if r.poly != nil {
		return r.poly.process(in)
	}
	if r.tab == nil {
		r.init()
	}
	r.buf = append(r.buf, in...)
	n := len(r.buf) / 2
	hw := int(math.Ceil(r.half))
	out := make([]int16, 0, int(float64(len(in))/r.step)+8)
	for {
		c := r.pos
		if int(math.Floor(c))+hw >= n {
			break // not enough input yet for this output frame
		}
		lo := int(math.Ceil(c - r.half))
		hi := int(math.Floor(c + r.half))
		var l, rt float64
		for i := lo; i <= hi; i++ {
			if i < 0 {
				continue // before the start of the stream
			}
			w := r.kernel(math.Abs(float64(i) - c))
			l += w * float64(r.buf[2*i])
			rt += w * float64(r.buf[2*i+1])
		}
		out = append(out, Clip16(l), Clip16(rt))
		r.pos += r.step
	}
	// Drop input we no longer need, keeping enough history for the next call.
	if drop := int(math.Floor(r.pos)) - hw - 1; drop > 0 {
		r.buf = r.buf[2*drop:]
		r.pos -= float64(drop)
	}
	return out
}
