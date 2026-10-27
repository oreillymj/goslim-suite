package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, content string) ([]Player, string, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadForms(t *testing.T) {
	good := map[string]string{
		"object with server": `{"server": "192.168.2.20:3483", "players": [{"name": "A", "mac": "94:9f:3e:8f:63:0c"}]}`,
		"trailing commas":    `{"players": [{"name": "A", "mac": "94:9f:3e:8f:63:0c"},]}`,
		"bare fragment":      `"players": [{"name": "A", "mac": "94-9F-3E-8F-63-0C"}]`,
		"bare array":         `[{"name": "A", "mac": "949f3e8f630c"}]`,
		"empty name allowed": `{"players": [{"name": "", "mac": "94:9f:3e:8f:63:0c"}]}`,
	}
	for name, c := range good {
		ps, _, err := load(t, c)
		if err != nil || len(ps) != 1 || ps[0].MAC != "94:9f:3e:8f:63:0c" {
			t.Errorf("%s: %+v, %v", name, ps, err)
		}
	}
	if _, srv, _ := load(t, good["object with server"]); srv != "192.168.2.20:3483" {
		t.Errorf("server = %q", srv)
	}
	bad := map[string]string{
		"missing name key": `{"players": [{"mac": "94:9f:3e:8f:63:0c"}]}`,
		"bad mac":          `{"players": [{"name": "A", "mac": "nonsense"}]}`,
		"duplicate mac":    `{"players": [{"name": "A", "mac": "94:9f:3e:8f:63:0c"}, {"name": "B", "mac": "94:9f:3e:8f:63:0c"}]}`,
		"bad port":         `{"players": [{"name": "A", "mac": "94:9f:3e:8f:63:0c", "http_port": 70000}]}`,
		"no players":       `{"players": []}`,
	}
	for name, c := range bad {
		if _, _, err := load(t, c); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestOptString(t *testing.T) {
	var o OptString
	if o.IsSet() {
		t.Fatal("set by default")
	}
	o.Set("true")
	if !o.IsSet() || o.Val != DefaultFile {
		t.Errorf("bare flag: %+v", o)
	}
	o.Set("mine.json")
	if o.Val != "mine.json" || !strings.HasSuffix(o.String(), ".json") {
		t.Errorf("named flag: %+v", o)
	}
}

func TestPerPlayerSwitches(t *testing.T) {
	ps, _, err := load(t, `{"players": [
		{"name": "A", "mac": "94:9f:3e:8f:63:0c", "decodeaudio": false, "resampleaudio": false, "autoresume": true},
		{"name": "B", "mac": "b8:e9:37:28:ae:dc"}]}`)
	if err != nil || len(ps) != 2 {
		t.Fatal(ps, err)
	}
	a, b := ps[0], ps[1]
	if a.DecodeAudio == nil || *a.DecodeAudio || a.ResampleAudio == nil || *a.ResampleAudio || a.AutoResume == nil || !*a.AutoResume {
		t.Errorf("A's switches were not read: %+v", a)
	}
	if b.DecodeAudio != nil || b.ResampleAudio != nil || b.AutoResume != nil {
		t.Errorf("B should inherit the global settings: %+v", b)
	}
}
