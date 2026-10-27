package decode

import (
	"os"
	"strings"
	"testing"
)

// TestProbeRealFiles probes real files. PROBE_FILES is a comma-separated list
// of kind:path, kind being m (MP3), f (FLAC), p (WAV/AIFF); each result is logged.
func TestProbeRealFiles(t *testing.T) {
	list := os.Getenv("PROBE_FILES")
	if list == "" {
		t.Skip("PROBE_FILES not set")
	}
	for _, ent := range strings.Split(list, ",") {
		kind, path, ok := strings.Cut(ent, ":")
		if !ok {
			t.Fatalf("bad entry %q", ent)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 64<<10 {
			b = b[:64<<10]
		}
		f := map[string]byte{"m": FormatMP3, "f": FormatFLAC, "p": FormatPCM}[kind]
		info, ok := Probe(f, b, [4]byte{'1', '1', '3', '1'})
		t.Logf("%-28s ok=%v  %s", path, ok, info)
		if !ok {
			t.Errorf("%s: probe failed", path)
		}
	}
}
