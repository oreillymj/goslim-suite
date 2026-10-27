package player

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
)

// DefaultName is the local-playback player name used when none is given: the
// host name plus a random number from 001 to 999.
func DefaultName() string {
	host, err := os.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i] // short name only
	}
	if err != nil || strings.TrimSpace(host) == "" {
		host = "GoSlim"
	}
	return fmt.Sprintf("%s-%03d", host, 1+rand.Intn(999))
}

// PickMAC returns the MAC address s if it is valid. Otherwise it derives one
// from the first network adapter that is up: locally administered, with the
// last byte XORed with salt so different programs on one PC get different
// players in LMS.
func PickMAC(s string, salt byte) net.HardwareAddr {
	if s != "" {
		if m, err := net.ParseMAC(s); err == nil && len(m) == 6 {
			return m
		}
	}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback == 0 && i.Flags&net.FlagUp != 0 && len(i.HardwareAddr) == 6 {
			m := append(net.HardwareAddr(nil), i.HardwareAddr...)
			m[0] |= 0x02 // locally administered, so it differs from the real NIC
			m[5] ^= salt
			return m
		}
	}
	return net.HardwareAddr{0x02, 0x00, 0x47, 0x6f, 0x53, 0x6c}
}
