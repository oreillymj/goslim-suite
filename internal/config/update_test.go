package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
  "server": "192.168.2.20:3483",
  "players": [
    {
      "name": "FamilyRoom2",
      "mac": "b8:e9:37:28:ae:dc",
      "http_port": 30000,
      "ip": "192.168.2.110",
      "model": "Sonos, Inc. Sonos Play:5",
    },
    {
      "name": "SunL",
      "mac": "94:9f:3e:8f:63:0c",
      "ip": "192.168.2.174",
      "devicename": "SunRoomL - ip \"x\" {not an object}"
    },
    { "name": "NoIP", "mac": "11:22:33:44:55:66" }
  ]
}
`

func TestUpdateIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(sample), 0o600)

	old, changed, err := UpdateIP(path, "94:9F:3E:8F:63:0C", "192.168.2.180")
	if err != nil || !changed || old != "192.168.2.174" {
		t.Fatalf("old %q changed %v err %v", old, changed, err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(sample, "192.168.2.174", "192.168.2.180", 1)
	if string(got) != want {
		t.Errorf("file changed beyond the ip value:\n%s", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("mode %v", st.Mode())
	}
	// the other player is untouched and a repeat is a no-op
	if _, changed, _ := UpdateIP(path, "94:9f:3e:8f:63:0c", "192.168.2.180"); changed {
		t.Error("rewrote an unchanged address")
	}
	if _, changed, _ := UpdateIP(path, "11:22:33:44:55:66", "10.0.0.1"); changed {
		t.Error("added an ip key that was not there")
	}
	if _, changed, _ := UpdateIP(path, "aa:aa:aa:aa:aa:aa", "10.0.0.1"); changed {
		t.Error("changed a player that is not in the file")
	}
	if _, _, err := Load(path); err != nil {
		t.Errorf("file no longer loads: %v", err)
	}
}
