// Package proxyproto is the wire protocol between GoSlim's proxy mode and
// GoUPnPAudioClient.
package proxyproto

// GoSlim proxy wire protocol, v1. This file is meant to be copied verbatim
// into GoSlim when proxy mode is added.
//
// Every frame is: [type:1][payload length:4, big endian][payload].
//
// Flow control: GoSlim must keep (bytes sent - Consumed) below a window
// (~2 MB, about 11 s of 44.1k/16/stereo). Consumed comes from the 'S' status
// frames and counts PCM bytes the renderer has pulled or that were discarded
// by a stop. This keeps the client's queue small so control frames are
// never stuck behind a backlog of audio.

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	// client -> GoSlim
	FrameHello  byte = 'H' // JSON Hello, sent once after connecting
	FrameStatus byte = 'S' // JSON Status, about once per second

	// GoSlim -> client
	FrameTrack  byte = 'T' // 44-byte WAV header (PCM) for the track that is starting
	FrameData   byte = 'D' // raw PCM bytes matching the last WAV header
	FrameEnd    byte = 'E' // decoder finished the current track (no more 'D' for it)
	FrameStop   byte = 'X' // stop / flush: drop everything queued
	FramePause  byte = 'P'
	FrameResume byte = 'R'
	FrameVolume byte = 'V' // 1 byte, 0..100
	FrameName   byte = 'N' // UTF-8 player name changed by LMS
)

const maxFrame = 1 << 20

type Hello struct {
	Version int    `json:"version"`
	MAC     string `json:"mac"`  // renderer MAC, aa:bb:cc:dd:ee:ff; used as the Slimproto MAC
	Name    string `json:"name"` // initial player name
}

type Status struct {
	State    string `json:"state"`    // playing | paused | stopped
	Consumed int64  `json:"consumed"` // cumulative PCM bytes pulled by the renderer or discarded
}

func WriteFrame(w io.Writer, t byte, p []byte) error {
	hdr := [5]byte{t}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	_, err := w.Write(append(hdr[:], p...)) // one Write so frames never interleave
	return err
}

func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}
