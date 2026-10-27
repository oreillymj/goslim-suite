package upnp

import (
	"fmt"
	"net"
	"os"
	"strings"
	"text/tabwriter"
)

// PrintRenderers prints the renderers found as a table.
func PrintRenderers(rs []*Renderer) {
	if len(rs) == 0 {
		fmt.Println("No UPnP audio renderers found.")
		fmt.Println("Check the firewall (allow this program on private networks), that you are on the")
		fmt.Println("same subnet, or try -bind <local IPv4> and a longer -t.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "MAC\tNAME\tIP\tMODEL")
	for _, r := range rs {
		mac := r.MAC
		if mac == "" {
			mac = "(unresolved)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mac, r.Name, r.IP, r.ModelText())
		if Verbose {
			fmt.Fprintf(tw, "\t  UDN: %s\n\t  AVTransport: %s\n", r.UDN, r.AVTransport.ControlURL)
		}
	}
	tw.Flush()
}

// ModelText is the manufacturer and model as one string.
func (r *Renderer) ModelText() string {
	if r.Manufacturer != "" {
		return r.Manufacturer + " " + r.Model
	}
	return r.Model
}

// ShortName turns a Sonos-style friendly name such as
// "Family Room - Sonos Play:5 Media Renderer" into "Family Room".
func ShortName(friendly string) string {
	if i := strings.Index(friendly, " - "); i >= 0 {
		friendly = friendly[:i]
	}
	return strings.TrimSpace(friendly)
}

// LocalIPFor returns the local address the OS would use to reach target.
func LocalIPFor(target net.IP) (net.IP, error) {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: target, Port: 9})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}
