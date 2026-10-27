package config

import (
	"encoding/json"
	"fmt"
	"os"

	"goslim/internal/upnp"
)

// Defaults are the values written into a generated config file for the
// settings that can also be given per player.
type Defaults struct {
	VolumeOffset  int
	VolumeMax     int
	BufferSeconds float64
	HTTPStartPort int // 0 = any free port; otherwise entry i gets HTTPStartPort+i
	DecodeAudio   bool
	ResampleAudio bool
	AutoResume    bool
	ICY           bool
	Server        string // LMS host[:port]; omitted when empty
}

// entry is one player in a generated file. name, mac and the settings are what
// Load reads; devicename, ip and model are descriptive and ignored on load.
type entry struct {
	Name          string  `json:"name"`
	MAC           string  `json:"mac"`
	VolumeOffset  int     `json:"volume_offset"`
	VolumeMax     int     `json:"volume_max"`
	BufferSeconds float64 `json:"buffer_seconds"`
	HTTPPort      int     `json:"http_port"`
	DecodeAudio   bool    `json:"decodeaudio"`
	ResampleAudio bool    `json:"resampleaudio"`
	AutoResume    bool    `json:"autoresume"`
	ICY           bool    `json:"icy"`
	DeviceName    string  `json:"devicename"`
	IP            string  `json:"ip"`
	Model         string  `json:"model"`
}

// Generate builds a config file for the renderers found: just the JSON, no
// headings. Renderers whose MAC could not be resolved are left out. It also
// returns the number of players written.
func Generate(rs []*upnp.Renderer, d Defaults) ([]byte, int) {
	out := struct {
		Server  string  `json:"server,omitempty"`
		Players []entry `json:"players"`
	}{Server: d.Server, Players: []entry{}}
	for _, r := range rs {
		if r.MAC == "" {
			continue
		}
		port := 0 // any free port
		if d.HTTPStartPort > 0 {
			port = d.HTTPStartPort + len(out.Players)
		}
		out.Players = append(out.Players, entry{
			Name: upnp.ShortName(r.Name), MAC: r.MAC,
			VolumeOffset: d.VolumeOffset, VolumeMax: d.VolumeMax, BufferSeconds: d.BufferSeconds, HTTPPort: port,
			DecodeAudio: d.DecodeAudio, ResampleAudio: d.ResampleAudio, AutoResume: d.AutoResume, ICY: d.ICY,
			DeviceName: r.Name, IP: r.IP.String(), Model: r.ModelText(),
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), len(out.Players)
}

// Write writes a generated config file to path (a relative path means the
// current directory). It never overwrites an existing file.
func Write(path string, rs []*upnp.Renderer, d Defaults) error {
	b, n := Generate(rs, d)
	if n == 0 {
		return fmt.Errorf("no renderers with a resolved MAC address to write")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; delete it or choose another name with -jsonfile=name.json", path)
		}
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// OptString is a flag that can be given bare (-jsonfile, meaning the default
// file name) or with a value (-jsonfile=name.json).
type OptString struct {
	set bool
	Val string
}

// DefaultFile is the file name used by a bare -jsonfile.
const DefaultFile = "config.json"

func (o *OptString) String() string { return o.Val }
func (o *OptString) Set(v string) error {
	o.set = true
	o.Val = v
	if v == "true" {
		o.Val = DefaultFile
	}
	return nil
}
func (o *OptString) IsBoolFlag() bool { return true }

// IsSet reports whether the flag was given.
func (o *OptString) IsSet() bool { return o.set }
