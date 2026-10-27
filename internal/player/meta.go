package player

// Track logging. Slimproto itself never tells a player what it is playing, so
// when a track starts we ask LMS's JSON-RPC interface (the web UI port) what
// this player's current track is.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type TrackInfo struct {
	pos, total                     int // playlist position (1-based) and length
	title, artist, album, tracknum string
	duration                       float64
	art                            string // cover art URL, if LMS has one
}

// ICY returns what to put in a stream's in-band metadata: "Artist - Title" and the
// cover art URL (either may be empty).
func (t *TrackInfo) ICY() (title, art string) {
	title = t.title
	if t.artist != "" && t.title != "" {
		title = t.artist + " - " + t.title
	}
	return title, t.art
}

func (t *TrackInfo) String() string {
	var b strings.Builder
	if t.pos > 0 {
		fmt.Fprintf(&b, "[%d/%d] ", t.pos, t.total)
	}
	if t.artist != "" {
		b.WriteString(t.artist + " - ")
	}
	if t.tracknum != "" {
		b.WriteString(t.tracknum + ". ")
	}
	if t.title == "" {
		b.WriteString("(untitled)")
	} else {
		b.WriteString(t.title)
	}
	var extra []string
	if t.album != "" {
		extra = append(extra, t.album)
	}
	if t.duration > 0 {
		d := int(t.duration + 0.5)
		extra = append(extra, fmt.Sprintf("%d:%02d", d/60, d%60))
	}
	if len(extra) > 0 {
		b.WriteString(" (" + strings.Join(extra, ", ") + ")")
	}
	return b.String()
}

func jstr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func jnum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// jsonrpc sends a command for this player to LMS's JSON-RPC interface and
// returns its result.
func (c *Client) jsonrpc(cmd []any) (map[string]any, error) {
	if c.opts.Web.Port == 0 {
		return nil, errors.New("track info disabled")
	}
	c.wmu.Lock()
	conn := c.conn
	c.wmu.Unlock()
	if conn == nil {
		return nil, errors.New("not connected to LMS")
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"id": 1, "method": "slim.request", "params": []any{c.mac.String(), cmd}})
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(c.opts.Web.Port)) + "/jsonrpc.js"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.Web.User != "" {
		req.SetBasicAuth(c.opts.Web.User, c.opts.Web.Pass)
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("LMS web interface wants a password (use -webuser/-webpass)")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("LMS web interface: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// LMSVolume asks LMS for this player's volume setting, 0-100. A negative value
// means the player is muted at that volume.
func (c *Client) LMSVolume() (int, error) {
	res, err := c.jsonrpc([]any{"mixer", "volume", "?"})
	if err != nil {
		return 0, err
	}
	v, ok := res["_volume"]
	if !ok {
		return 0, errors.New("no volume in the reply")
	}
	return int(jnum(v)), nil
}

// fetchTrack asks LMS for this player's current track.
func (c *Client) fetchTrack() (*TrackInfo, error) {
	res, err := c.jsonrpc([]any{"status", "-", 1, "tags:altdcK"})
	if err != nil {
		return nil, err
	}
	out := struct{ Result map[string]any }{Result: res}
	loop, _ := out.Result["playlist_loop"].([]any)
	if len(loop) == 0 {
		return nil, errors.New("LMS reports an empty playlist")
	}
	item, _ := loop[0].(map[string]any)
	ti := &TrackInfo{
		title:    jstr(item["title"]),
		artist:   jstr(item["artist"]),
		album:    jstr(item["album"]),
		tracknum: jstr(item["tracknum"]),
		duration: jnum(item["duration"]),
		total:    int(jnum(out.Result["playlist_tracks"])),
	}
	ti.art = c.artURL(item)
	if v, ok := out.Result["playlist_cur_index"]; ok {
		ti.pos = int(jnum(v)) + 1
	}
	return ti, nil
}

// FetchLater looks up the track LMS has just switched to. It waits a moment
// for LMS to process our "track started" report, then yields the result
// (nil on failure) on the returned channel.
func (c *Client) FetchLater() <-chan *TrackInfo {
	ch := make(chan *TrackInfo, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		ti, err := c.fetchTrack()
		if err != nil {
			log.Printf("[%s] track info unavailable: %v", c.name, err)
			ti = nil
		}
		ch <- ti
	}()
	return ch
}

// Announce logs a track transition. at is when the track became audible;
// queued is when LMS switched to it (earlier in proxy mode, because the
// renderer is fed ahead of playback).
func (c *Client) Announce(at, queued time.Time, info <-chan *TrackInfo) {
	var ti *TrackInfo
	select {
	case ti = <-info:
	case <-time.After(3 * time.Second):
	}
	lead := ""
	if d := at.Sub(queued); d >= time.Second {
		lead = fmt.Sprintf("  (LMS switched %.1fs earlier)", d.Seconds())
	}
	if ti == nil {
		log.Printf("[%s] %s track started%s", c.name, at.Format("15:04:05.0"), lead)
		return
	}
	log.Printf("[%s] %s now playing: %s%s", c.name, at.Format("15:04:05.0"), ti, lead)
}

// artURL is where the renderer can fetch the track's cover art: the remote URL
// LMS knows for a stream, else the cover LMS serves on its web port.
func (c *Client) artURL(item map[string]any) string {
	c.wmu.Lock()
	conn := c.conn
	c.wmu.Unlock()
	if conn == nil || c.opts.Web.Port == 0 {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return ""
	}
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(c.opts.Web.Port))
	if u := jstr(item["artwork_url"]); u != "" {
		if strings.HasPrefix(u, "/") {
			return base + u
		}
		return u
	}
	if id := jstr(item["coverid"]); id != "" && id != "0" {
		return base + "/music/" + id + "/cover.jpg"
	}
	return ""
}
