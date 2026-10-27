package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFormatMatrix decodes the files made by hack/formats/gen.py and prints the
// rows of the README's format-support table. It is skipped unless
// DECODE_MATRIX_DIR points at that directory; FFMPEG enables the "ffmpeg" rows and
// DECODE_MATRIX_OUT names a file to write the result to.
func TestFormatMatrix(t *testing.T) {
	dir := os.Getenv("DECODE_MATRIX_DIR")
	if dir == "" {
		t.Skip("set DECODE_MATRIX_DIR (see hack/formats/gen.py) to run")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	ffmpeg := os.Getenv("FFMPEG")
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		name, file, kind, letter, label, rate, mode, refFile := f[0], f[1], f[2], f[3][0], f[4], f[5], f[6], f[7]
		var hint [4]byte // raw PCM: the strm command's size, rate, channels and endianness characters
		if len(f) > 8 {
			copy(hint[:], f[8])
		}
		data, _ := os.ReadFile(filepath.Join(dir, file))
		refB, _ := os.ReadFile(filepath.Join(dir, refFile))
		ref := make([]int16, len(refB)/2)
		for i := range ref {
			ref[i] = int16(binary.LittleEndian.Uint16(refB[i*2:]))
		}
		d := &Decoder{OutRate: 48000, FFmpeg: ffmpeg}
		if kind == "ffmpeg" && ffmpeg == "" {
			continue
		}

		var best time.Duration
		var got []int16
		var gotRate int
		var derr error
		for run := 0; run < 3; run++ {
			got, gotRate = got[:0], 0
			start := time.Now()
			derr = d.Decode(letter, bytes.NewReader(data), hint, func(s []int16, r int) error {
				got = append(got, s...)
				gotRate = r
				return nil
			})
			if el := time.Since(start); run == 0 || el < best {
				best = el
			}
			if derr != nil {
				break
			}
		}
		var rateHz float64
		fmt.Sscanf(rate, "%f", &rateHz)
		secs := float64(len(ref)/2) / rateHz
		result := ""
		switch {
		case derr != nil:
			msg := strings.ReplaceAll(strings.TrimSpace(derr.Error()), "\n", " ")
			if len(msg) > 70 {
				msg = msg[:70] + "…"
			}
			result = "**not decoded:** " + msg
		case kind == "exact":
			if len(got) != len(ref) {
				result = fmt.Sprintf("**length differs:** %d vs %d samples", len(got)/2, len(ref)/2)
			} else {
				bad := -1
				for i := range got {
					if got[i] != ref[i] {
						bad = i
						break
					}
				}
				if bad >= 0 {
					result = fmt.Sprintf("**differs** at sample %d", bad/2)
				} else {
					result = "bit-exact"
				}
			}
		default: // lossy: compare with ffmpeg's decode
			n := len(got)
			if len(ref) < n {
				n = len(ref)
			}
			var maxd int
			var sr, sd float64
			for i := 0; i < n; i++ {
				diff := int(got[i]) - int(ref[i])
				if diff < 0 {
					diff = -diff
				}
				if diff > maxd {
					maxd = diff
				}
				sr += float64(ref[i]) * float64(ref[i])
				sd += float64(diff) * float64(diff)
			}
			snr := 99.0
			if sd > 0 {
				snr = 10 * math.Log10(sr/sd)
			}
			if gotRate != 0 && fmt.Sprint(gotRate) != rate && kind == "lossy" {
				result = fmt.Sprintf("**rate %d**", gotRate)
			} else if maxd <= 1 {
				result = "matches ffmpeg (±1 LSB)"
			} else {
				result = fmt.Sprintf("max diff %d LSB, %.0f dB vs ffmpeg", maxd, snr)
			}
			if d := len(got)/2 - len(ref)/2; d != 0 {
				result += fmt.Sprintf(", %+d frames", d)
			}
		}
		speed := ""
		if derr == nil && best > 0 {
			speed = fmt.Sprintf("%.0f×", secs/best.Seconds())
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |", label, rate, mode, result, speed, name))
		t.Logf("%s", rows[len(rows)-1])
	}
	if out := os.Getenv("DECODE_MATRIX_OUT"); out != "" {
		os.WriteFile(out, []byte("| Format | Rate (Hz) | Depth / mode | Result | Speed | id |\n|---|---|---|---|---|---|\n"+strings.Join(rows, "\n")+"\n"), 0o644)
	}
}
