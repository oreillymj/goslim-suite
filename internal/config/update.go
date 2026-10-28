package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"goslim/internal/upnp"
)

var (
	updateMu sync.Mutex
	macKeyRe = regexp.MustCompile(`"mac"\s*:\s*"([^"]*)"`)
	ipKeyRe  = regexp.MustCompile(`("ip"\s*:\s*")([^"]*)(")`)
)

// objectSpan is the byte range [start,end) of one {...} in the file.
type objectSpan struct{ start, end int }

// objectSpans lists every JSON object in txt (strings are skipped over).
func objectSpans(txt string) []objectSpan {
	var spans []objectSpan
	var stack []int
	inStr := false
	for i := 0; i < len(txt); i++ {
		c := txt[i]
		switch {
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			stack = append(stack, i)
		case c == '}' && len(stack) > 0:
			spans = append(spans, objectSpan{stack[len(stack)-1], i + 1})
			stack = stack[:len(stack)-1]
		}
	}
	return spans
}

// UpdateIP rewrites the "ip" value of the player with the given MAC in the config
// file if it differs from ip. The "ip" key is informational (players are found by
// MAC), so only an existing one is changed; the rest of the file, formatting and
// comments-free quirks included, is left exactly as it was. It returns the old
// value and whether the file was changed.
func UpdateIP(path, mac, ip string) (old string, changed bool, err error) {
	updateMu.Lock()
	defer updateMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	txt := string(b)
	spans := objectSpans(txt)
	for _, m := range macKeyRe.FindAllStringSubmatchIndex(txt, -1) {
		if upnp.NormMAC(txt[m[2]:m[3]]) != upnp.NormMAC(mac) {
			continue
		}
		// the smallest object containing this "mac" key is the player entry
		best := objectSpan{-1, 0}
		for _, s := range spans {
			if s.start <= m[0] && m[1] <= s.end && (best.start < 0 || s.end-s.start < best.end-best.start) {
				best = s
			}
		}
		if best.start < 0 {
			continue
		}
		loc := ipKeyRe.FindStringSubmatchIndex(txt[best.start:best.end])
		if loc == nil {
			return "", false, nil // no "ip" key to keep up to date
		}
		valStart, valEnd := best.start+loc[4], best.start+loc[5]
		old = txt[valStart:valEnd]
		if old == ip {
			return old, false, nil
		}
		out := txt[:valStart] + ip + txt[valEnd:]
		return old, true, writeFileAtomic(path, []byte(out))
	}
	return "", false, nil
}

func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
