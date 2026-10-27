//go:build !unix

package upnpout

// cpuSeconds is not measured on this platform; so the idle-CPU check passes trivially.
func cpuSeconds() float64 { return 0 }
