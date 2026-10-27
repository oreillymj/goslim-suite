package upnp

// -info: print what a Sonos renderer says about its zone group / stereo pair
// and its per-channel volume and mute, to diagnose channel problems.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var (
	upnpErrRe = regexp.MustCompile(`<errorCode>\s*(\d+)\s*</errorCode>`)
	volRe     = regexp.MustCompile(`<CurrentVolume>([^<]*)</CurrentVolume>`)
	muteRe    = regexp.MustCompile(`<CurrentMute>([^<]*)</CurrentMute>`)
)

// call is like soap but returns the response body.
func (u *Client) call(svc *Service, action string, args ...[2]string) (string, error) {
	if svc == nil {
		return "", fmt.Errorf("renderer has no service for %s", action)
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
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header["SOAPACTION"] = []string{fmt.Sprintf(`"%s#%s"`, svc.Type, action)}
	resp, err := u.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		if m := upnpErrRe.FindSubmatch(body); m != nil {
			return "", fmt.Errorf("UPnP error %s", m[1])
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

type zgMember struct {
	UUID            string     `xml:"UUID,attr"`
	Location        string     `xml:"Location,attr"`
	ZoneName        string     `xml:"ZoneName,attr"`
	ChannelMapSet   string     `xml:"ChannelMapSet,attr"`
	HTSatChanMapSet string     `xml:"HTSatChanMapSet,attr"`
	Invisible       string     `xml:"Invisible,attr"`
	Satellites      []zgMember `xml:"Satellite"`
}

type zgGroup struct {
	Coordinator string     `xml:"Coordinator,attr"`
	ID          string     `xml:"ID,attr"`
	Members     []zgMember `xml:"ZoneGroupMember"`
}

type zgState struct {
	Groups []zgGroup `xml:"ZoneGroups>ZoneGroup"`
}

func printMember(m zgMember, indent string, self string) {
	tag := ""
	if self != "" && strings.Contains(self, m.UUID) {
		tag = "   <-- this renderer"
	}
	fmt.Printf("%s%q  %s%s\n", indent, m.ZoneName, m.UUID, tag)
	fmt.Printf("%s    location: %s\n", indent, m.Location)
	if m.Invisible != "" {
		fmt.Printf("%s    invisible: %s\n", indent, m.Invisible)
	}
	if m.ChannelMapSet != "" {
		fmt.Printf("%s    ChannelMapSet: %s\n", indent, m.ChannelMapSet)
	}
	if m.HTSatChanMapSet != "" {
		fmt.Printf("%s    HTSatChanMapSet: %s\n", indent, m.HTSatChanMapSet)
	}
	for _, s := range m.Satellites {
		fmt.Printf("%s    satellite:\n", indent)
		printMember(s, indent+"      ", self)
	}
}

func PrintInfo(u *Client) {
	r := u.R
	fmt.Printf("Renderer: %s\nMAC: %s   IP: %s\nUDN: %s\n\n", r.Name, r.MAC, r.IP, r.UDN)

	fmt.Println("Zone groups (as this speaker reports them):")
	if r.ZoneTopology == nil {
		fmt.Println("  no ZoneGroupTopology service (not a Sonos device?)")
	} else if body, err := u.call(r.ZoneTopology, "GetZoneGroupState"); err != nil {
		fmt.Printf("  GetZoneGroupState failed: %v\n", err)
	} else {
		var env struct {
			Body struct {
				Resp struct {
					State string `xml:"ZoneGroupState"`
				} `xml:",any"`
			} `xml:"Body"`
		}
		var st zgState
		if err := xml.Unmarshal([]byte(body), &env); err != nil {
			fmt.Printf("  cannot parse response: %v\n", err)
		} else if err := xml.Unmarshal([]byte(env.Body.Resp.State), &st); err != nil {
			fmt.Printf("  cannot parse zone group state: %v\n", err)
		} else {
			for _, g := range st.Groups {
				fmt.Printf("  group %s, coordinator %s\n", g.ID, g.Coordinator)
				for _, m := range g.Members {
					printMember(m, "    - ", r.UDN)
				}
			}
			if Verbose {
				fmt.Printf("\nraw state:\n%s\n", env.Body.Resp.State)
			}
		}
	}

	fmt.Println("\nVolume and mute per channel (LF/RF only exist on a stereo pair):")
	for _, ch := range []string{"Master", "LF", "RF"} {
		args := [][2]string{{"InstanceID", "0"}, {"Channel", ch}}
		vol, mute := "?", "?"
		vb, verr := u.call(r.RenderingControl, "GetVolume", args...)
		if verr != nil {
			vol = "error: " + verr.Error()
		} else if m := volRe.FindStringSubmatch(vb); m != nil {
			vol = m[1]
		}
		mb, merr := u.call(r.RenderingControl, "GetMute", args...)
		if merr != nil {
			mute = "error: " + merr.Error()
		} else if m := muteRe.FindStringSubmatch(mb); m != nil {
			mute = m[1]
		}
		fmt.Printf("  %-6s volume %-24s mute %s\n", ch, vol, mute)
	}
}
