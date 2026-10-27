package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"goslim/internal/mdns"
	"goslim/internal/proxyproto"
	"goslim/internal/uclient"
	"goslim/internal/upnp"
)

// run does what the command line asked for and returns when ctx is cancelled
// (or immediately for the one-shot modes).
func run(ctx context.Context, f *flags) error {
	upnp.Verbose = f.verbose
	addr := net.JoinHostPort(f.host, strconv.Itoa(f.port))
	discover := func() (string, error) { return findGoSlim(ctx, f) }
	if !f.mdns {
		discover = nil
	}

	if f.test {
		mac := upnp.NormMAC(f.mac)
		if mac == "" {
			mac = "02:00:47:6f:54:31" // made-up MAC; -m overrides it
		}
		name := f.name
		if name == "" {
			name = "GoSlim Test"
		}
		log.Printf("test mode: playing GoSlim's audio locally as %q (%s)", name, mac)
		sess := uclient.NewTest(name, f.idle)
		sess.Discover = discover
		sess.ConnectLoop(addr, proxyproto.Hello{Version: 1, MAC: mac, Name: name}, ctx.Done())
		return nil
	}

	renderers := upnp.DiscoverRenderers(f.bind, f.timeout)
	if f.list || f.mac == "" {
		upnp.PrintRenderers(renderers)
		return nil
	}
	want := upnp.NormMAC(f.mac)
	if want == "" {
		return fmt.Errorf("-m: %q is not a valid MAC address", f.mac)
	}
	var target *upnp.Renderer
	for _, r := range renderers {
		if r.MAC == want {
			target = r
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "No renderer with MAC %s found. Available renderers:\n", want)
		upnp.PrintRenderers(renderers)
		return errors.New("renderer not found")
	}
	if f.info {
		upnp.PrintInfo(upnp.NewClient(target, target.Name))
		return nil
	}
	name := f.name
	if name == "" {
		if name = upnp.ShortName(target.Name); name == "" {
			name = target.Name
		}
	}

	lip, err := upnp.LocalIPFor(target.IP)
	if err != nil {
		return fmt.Errorf("cannot determine local address towards %s: %w", target.IP, err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(lip.String(), strconv.Itoa(f.httpPort)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	baseURL := "http://" + ln.Addr().String()
	log.Printf("renderer %q (%s) at %s; serving audio on %s", target.Name, target.MAC, target.IP, baseURL)

	sess := uclient.New(uclient.Options{
		Renderer: target, Name: name, BaseURL: baseURL, Clen: f.clen, Idle: f.idle,
		VolOffset: f.volOffset, VolMax: f.volMax,
	})
	sess.Discover = discover
	srv := &http.Server{Handler: sess}
	go srv.Serve(ln)
	defer srv.Close()
	sess.ConnectLoop(addr, proxyproto.Hello{Version: 1, MAC: target.MAC, Name: name}, ctx.Done())
	return nil
}

// findGoSlim looks for a GoSlim proxy with mDNS, preferring one at -host and, with
// -mdnsname, only taking one whose name contains that text.
func findGoSlim(ctx context.Context, f *flags) (string, error) {
	es, err := mdns.Browse(ctx, 3*time.Second)
	if err != nil {
		return "", err
	}
	var best *mdns.Entry
	for i, e := range es {
		if f.mdnsName != "" && !strings.Contains(strings.ToLower(e.Instance), strings.ToLower(f.mdnsName)) {
			continue
		}
		if best == nil {
			best = &es[i]
		}
		if e.IP.String() == f.host {
			best = &es[i]
			break
		}
	}
	if best == nil {
		return "", errors.New("no matching GoSlim proxy answered")
	}
	return best.Addr(), nil
}
