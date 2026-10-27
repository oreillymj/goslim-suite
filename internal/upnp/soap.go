package upnp

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

type Client struct {
	HC    *http.Client
	R     *Renderer
	Label string // player name, for log lines
}

func (u *Client) soap(svc *Service, action string, args ...[2]string) error {
	if svc == nil {
		return fmt.Errorf("renderer has no service for %s", action)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>`+
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`+
		`<s:Body><u:%s xmlns:u="%s">`, action, svc.Type)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a[0])
		xml.EscapeText(&b, []byte(a[1]))
		fmt.Fprintf(&b, "</%s>", a[0])
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequest("POST", svc.ControlURL, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header["SOAPACTION"] = []string{fmt.Sprintf(`"%s#%s"`, svc.Type, action)} // keep exact casing
	resp, err := u.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d: %s", action, resp.StatusCode, body)
	}
	if action != "SetVolume" { // the caller logs that, with the LMS volume
		log.Printf("[%s] UPnP %s ok", u.Label, action)
	}
	return nil
}

func (u *Client) SetURI(uri, meta string) error {
	return u.soap(u.R.AVTransport, "SetAVTransportURI",
		[2]string{"InstanceID", "0"}, [2]string{"CurrentURI", uri}, [2]string{"CurrentURIMetaData", meta})
}
func (u *Client) Play() error {
	return u.soap(u.R.AVTransport, "Play", [2]string{"InstanceID", "0"}, [2]string{"Speed", "1"})
}
func (u *Client) Pause() error {
	return u.soap(u.R.AVTransport, "Pause", [2]string{"InstanceID", "0"})
}
func (u *Client) Stop() error {
	return u.soap(u.R.AVTransport, "Stop", [2]string{"InstanceID", "0"})
}
func (u *Client) SetVolume(v int) error {
	if u.R.RenderingControl == nil {
		return nil
	}
	return u.soap(u.R.RenderingControl, "SetVolume",
		[2]string{"InstanceID", "0"}, [2]string{"Channel", "Master"}, [2]string{"DesiredVolume", strconv.Itoa(v)})
}

var transportRe = regexp.MustCompile(`<CurrentTransportState>([^<]*)</CurrentTransportState>`)

// transportState returns PLAYING, PAUSED_PLAYBACK, STOPPED, TRANSITIONING, ...
func (u *Client) TransportState() (string, error) {
	body, err := u.call(u.R.AVTransport, "GetTransportInfo", [2]string{"InstanceID", "0"})
	if err != nil {
		return "", err
	}
	if m := transportRe.FindStringSubmatch(body); m != nil {
		return m[1], nil
	}
	return "", errors.New("no transport state in response")
}

func DIDL(title, uri string) string { return DIDLMime(title, uri, "audio/wav") }

// DIDLMime is DIDL for a stream of the given MIME type.
func DIDLMime(title, uri, mime string) string {
	var t, u bytes.Buffer
	xml.EscapeText(&t, []byte(title))
	xml.EscapeText(&u, []byte(uri))
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item id="0" parentID="-1" restricted="1">` +
		`<dc:title>` + t.String() + `</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<res protocolInfo="http-get:*:` + mime + `:*">` + u.String() + `</res></item></DIDL-Lite>`
}

// NewClient returns a SOAP client for r. label is used in log lines.
func NewClient(r *Renderer, label string) *Client {
	return &Client{HC: &http.Client{Timeout: 10 * time.Second}, R: r, Label: label}
}
