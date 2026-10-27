package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"

	"goslim/internal/decode"
	"goslim/internal/lms"
	"goslim/internal/mdns"
	"goslim/internal/player"
	"goslim/internal/proxy"
)

// run does what the command line asked for and returns when ctx is cancelled.
func run(ctx context.Context, f *flags) error {
	finder := &lms.Finder{}
	finder.SetServer(f.server, f.server != "")
	if f.stereoTest != "" && f.proxyPort == 0 {
		return errors.New("-stereotest needs -proxyport")
	}
	dec := &decode.Decoder{OutRate: f.rate, NoNativeAAC: !f.nativeAAC, NoNativeALAC: !f.nativeALAC, NoNativeVorbis: !f.nativeOgg}
	if f.ffmpeg != "" {
		p, err := decode.FindFFmpeg(f.ffmpeg)
		if err != nil {
			return fmt.Errorf("-ffmpeg: %w", err)
		}
		dec.FFmpeg = p
	}
	newPlayer := func(name string, mac net.HardwareAddr) *player.Client {
		return player.New(player.Options{
			Name: name, MAC: mac, OutRate: f.rate, MaxRate: f.maxRate, Decoder: dec, Finder: finder,
			ReplayGain: f.replayGain, Verbose: f.verbose, Web: player.WebConfig{Port: f.webPort, User: f.webUser, Pass: f.webPass},
		})
	}
	log.Printf("formats offered to LMS: %s", dec.Caps())

	if f.proxyPort != 0 {
		ln, err := listenProxy(f.proxyPort, f.mdns)
		if err != nil {
			return err
		}
		defer ln.Close()
		if f.mdns {
			port := ln.Addr().(*net.TCPAddr).Port
			inst := f.mdnsName
			if inst == "" {
				h, _ := os.Hostname()
				inst = fmt.Sprintf("GoSlim %s %d", h, port)
			}
			if err := mdns.Advertise(ctx, mdns.Service{Instance: inst, Port: port, Text: []string{"ver=1"}}); err != nil {
				log.Printf("%v (continuing without mDNS: clients must use port %d)", err, port)
			}
		}
		srv := &proxy.Server{Rate: f.rate, NewPlayer: newPlayer}
		if f.stereoTest != "" {
			st, err := proxy.LoadStereoTest(f.stereoTest, f.rate)
			if err != nil {
				return fmt.Errorf("stereotest: %w", err)
			}
			srv.StereoTest = st
		}
		return srv.Run(ctx, ln)
	}

	// Default: play on this PC's sound device.
	name := f.name
	if name == "" {
		name = player.DefaultName()
	}
	c := newPlayer(name, player.PickMAC(f.mac, 0x5a))
	closer, err := c.PlayLocal()
	if err != nil {
		return fmt.Errorf("audio init: %w", err)
	}
	defer closer.Close()
	log.Printf("player %q, MAC %s, output %d Hz", c.Name(), c.MAC(), f.rate)
	go c.Loop()
	<-ctx.Done()
	c.Close()
	return nil
}

// listenProxy opens the proxy's TCP port. When it is taken (or port is -1) and mDNS
// will announce the real port, any free port is used instead.
func listenProxy(port int, mdnsOn bool) (net.Listener, error) {
	if port > 0 {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err == nil || !mdnsOn {
			return ln, err
		}
		log.Printf("proxy port %d is in use (%v); using a free port, which mDNS announces", port, err)
	} else if !mdnsOn {
		return nil, errors.New("-proxyport -1 needs -mdns, otherwise clients cannot find the port")
	}
	return net.Listen("tcp", ":0")
}
