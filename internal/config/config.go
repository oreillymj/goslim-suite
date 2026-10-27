// Package config reads and writes the GoSlimPlayer config file: the LMS
// server address and the list of UPnP renderers to drive, each presented to LMS
// as its own player.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"goslim/internal/upnp"
)

// Player is one validated entry from the config file.
type Player struct {
	Name          string // may be empty: startPlayer then derives it from the renderer's own name
	MAC           string
	VolumeOffset  *int
	VolumeMax     *int
	Buffer        *float64
	HTTPPort      *int
	DecodeAudio   *bool // nil = use the global setting
	ResampleAudio *bool
	AutoResume    *bool
	ICY           *bool
}

type rawPlayer struct {
	Name          *string  `json:"name"` // the key must be present; its value may be empty
	MAC           string   `json:"mac"`
	VolumeOffset  *int     `json:"volume_offset"`
	VolumeMax     *int     `json:"volume_max"`
	Buffer        *float64 `json:"buffer_seconds"`
	HTTPPort      *int     `json:"http_port"`
	DecodeAudio   *bool    `json:"decodeaudio"`
	ResampleAudio *bool    `json:"resampleaudio"`
	AutoResume    *bool    `json:"autoresume"`
	ICY           *bool    `json:"icy"`
	Enabled       *bool    `json:"enabled"`
}

var trailingComma = regexp.MustCompile(`,\s*([\]}])`)

// Load reads the players file. It is forgiving about trailing commas
// and about a bare `"players": [...]` fragment without the outer braces.
func Load(path string) ([]Player, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	txt := trailingComma.ReplaceAllString(strings.TrimSpace(string(b)), "$1")
	if txt != "" && txt[0] != '{' && txt[0] != '[' {
		txt = "{" + txt + "}"
	}
	var cfg struct {
		Server  string      `json:"server"` // LMS host[:port]; optional
		Players []rawPlayer `json:"players"`
	}
	if txt != "" && txt[0] == '[' {
		if err := json.Unmarshal([]byte(txt), &cfg.Players); err != nil {
			return nil, "", err
		}
	} else if err := json.Unmarshal([]byte(txt), &cfg); err != nil {
		return nil, "", err
	}

	seen := map[string]bool{}
	var out []Player
	for i, p := range cfg.Players {
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		m := upnp.NormMAC(p.MAC)
		if m == "" {
			return nil, "", fmt.Errorf("player %d: %q is not a valid MAC address", i+1, p.MAC)
		}
		if p.Name == nil {
			return nil, "", fmt.Errorf("player %d (%s): the \"name\" field is required (it may be left empty to use the renderer's own name)", i+1, m)
		}
		if p.HTTPPort != nil && (*p.HTTPPort < 0 || *p.HTTPPort > 65535) {
			return nil, "", fmt.Errorf("player %d (%s): http_port %d is out of range", i+1, m, *p.HTTPPort)
		}
		if seen[m] {
			return nil, "", fmt.Errorf("MAC %s is listed twice", m)
		}
		seen[m] = true
		out = append(out, Player{Name: strings.TrimSpace(*p.Name), MAC: m, VolumeOffset: p.VolumeOffset, VolumeMax: p.VolumeMax, Buffer: p.Buffer, HTTPPort: p.HTTPPort,
			DecodeAudio: p.DecodeAudio, ResampleAudio: p.ResampleAudio, AutoResume: p.AutoResume, ICY: p.ICY})
	}
	if len(out) == 0 {
		return nil, "", errors.New("no enabled players in the file")
	}
	return out, cfg.Server, nil
}
