//go:build windows

package upnp

import (
	"net"
	"syscall"
	"unsafe"
)

var procSendARP = syscall.NewLazyDLL("iphlpapi.dll").NewProc("SendARP")

// lookupMAC resolves an on-link IPv4 address to a MAC using iphlpapi!SendARP.
func lookupMAC(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	// IPAddr is the address in network byte order, i.e. a.b.c.d in memory order.
	dest := uint32(ip4[0]) | uint32(ip4[1])<<8 | uint32(ip4[2])<<16 | uint32(ip4[3])<<24
	var mac [8]byte
	macLen := uint32(len(mac))
	r, _, _ := procSendARP.Call(uintptr(dest), 0, uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&macLen)))
	if r != 0 || macLen < 6 {
		return ""
	}
	return net.HardwareAddr(mac[:6]).String()
}
