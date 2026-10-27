//go:build !windows

package upnp

import (
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var arpLine = regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\) at ([0-9a-fA-F:]+)`)

// lookupMAC resolves an IPv4 address using the OS ARP cache.
func lookupMAC(ip net.IP) string {
	want := ip.String()
	if b, err := os.ReadFile("/proc/net/arp"); err == nil { // Linux
		for _, ln := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(ln)
			if len(f) >= 4 && f[0] == want && f[3] != "00:00:00:00:00:00" {
				return f[3]
			}
		}
	}
	out, err := exec.Command("arp", "-an").Output() // macOS / BSD
	if err != nil {
		return ""
	}
	for _, m := range arpLine.FindAllStringSubmatch(string(out), -1) {
		if m[1] == want {
			parts := strings.Split(m[2], ":")
			for i, p := range parts {
				if len(p) == 1 {
					parts[i] = "0" + p // macOS drops leading zeros
				}
			}
			return strings.Join(parts, ":")
		}
	}
	return ""
}
