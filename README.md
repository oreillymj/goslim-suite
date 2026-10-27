# GoSlim suite

Squeezebox (Slimproto) players for Lyrion / Logitech Media Server, written in Go.

| Command | What it does |
|---|---|
| `goslimplayer` | Plays on the local sound device, or with `-u config.json` presents each UPnP/Sonos renderer in the file to LMS as its own player and streams straight to it. |
| `goslim` | Plays locally, or with `-proxyport` accepts `goupnpaudioclient` connections and presents each as a player. |
| `goupnpaudioclient` | Lists UPnP renderers; with `-m MAC` streams a GoSlim proxy player to that renderer. |

`goslimplayer` supersedes the other two for most uses; they remain for the proxy setup.

## Layout

```
cmd/<app>/          main.go (version + startup), flags.go (command line), playback.go (what the app does)
internal/app        process harness: version banner, logging, SIGINT/SIGTERM -> context
internal/lms        finds the LMS server once and shares it between players
internal/player     the Slimproto client: protocol, track queue, ReplayGain, track-change log
internal/decode     MP3, FLAC, WAV/AIFF/raw PCM, AAC-LC (go-aac), ALAC, Ogg Vorbis, own MP4 reader, optional ffmpeg (HE-AAC), resampler
internal/alac       ALAC decoder, vendored from alicebob/alac with fixes (see its header)
internal/audio      local sound output (oto); a stub when built with -tags noaudio
internal/upnp       renderer discovery, SOAP control, diagnostics, the HTTP stream and pacer
internal/upnpout    feeds a player's audio to a UPnP renderer (goslimplayer)
internal/config     the config file and the -jsonfile generator
internal/proxyproto the wire protocol between goslim and goupnpaudioclient
internal/proxy      goslim's proxy mode
internal/mdns       small mDNS/DNS-SD responder and browser (the proxy port announcement)
internal/uclient    goupnpaudioclient's engine
docker/             Dockerfile for building in a Go 1.26 container
hack/               build.sh, build.bat, docker_build.sh, itest.py, formats/gen.py (test files for the format table)
deploy/             systemd units, NSSM install script
dist/               build output (git-ignored)
```

## Building

Needs **Go 1.26 or newer** (the AAC decoder requires it; with the default `GOTOOLCHAIN=auto` the `go` command fetches it).
The build scripts first run `go get` for each external dependency, then `go mod tidy`. The programs print their version
at startup and with `-version`; it is injected with `-ldflags "-s -w -X main.version=<VERSION>"`. Without `VERSION` it
is taken from `git describe`, else `dev`. Output goes to `dist/<app>-<version>-<os>-<arch>`.

The dependencies are oto (local audio), go-mp3, mewkiz/flac, go-aac, jfreymuth/oggvorbis and golang.org/x/net (mDNS).
`build.sh` pins the versions it was tested with (`DEPS` at its top); `build.bat` fetches the latest.

### Windows

`hack\build.bat` builds all three programs for Windows and Linux, 64-bit and 32-bit (`amd64`, `386`). The Linux builds
are made with `-tags noaudio` (UPnP and proxy modes only). The outputs are plain `.exe` files and Linux binaries.

```
hack\build.bat /?                   show this help
```

To build with version information:

In a command prompt (cmd)

```
set "VERSION=0.5" && hack\build.bat
```

In a PowerShell terminal

```
$env:VERSION = "0.5"
.\hack\build.bat
```

Add `set SKIP_DEPS=1` (cmd) or `$env:SKIP_DEPS = "1"` (PowerShell) to skip `go get` and `go mod tidy` and build offline from
the dependencies already downloaded. macOS and Linux arm64 builds need `build.sh`, from WSL or Git Bash.

### Linux and macOS

`hack/build.sh` builds every app for Windows (`amd64`, `386`), Linux (`amd64`, `arm64`) and macOS (`arm64`, `amd64`).
Linux and macOS outputs are gzipped as `.gz`; Windows ones stay `.exe`.

```
hack/build.sh                                        # every app, every target
VERSION=0.5 hack/build.sh                            # with version information
APPS=goslimplayer TARGETS="linux/arm64" hack/build.sh   # one app, one target
SKIP_DEPS=1 hack/build.sh                            # offline: skip go get / go mod tidy
```

### Docker

`hack/docker_build.sh` builds everything in a `golang:1.26` container, so the host needs only Docker, and copies the
results into `./dist` (owned by you, not root):

```
hack/docker_build.sh                          # version from git describe (else "dev"), Linux builds without local audio
VERSION=0.5 hack/docker_build.sh              # with version information
NOAUDIO=0 hack/docker_build.sh                # link ALSA local audio into the Linux binaries
APPS=goslimplayer TARGETS="linux/amd64" hack/docker_build.sh
```

It runs `docker build -f docker/Dockerfile` with build arguments `VERSION`, `NOAUDIO` (1 = `-tags noaudio`, the
default; 0 = `LINUX_AUDIO=1`), `APPS` and `TARGETS`, then `docker run -v ./dist:/out`, whose command copies the
build's `dist/` to the mounted directory. The same two commands work by hand (see the top of `docker/Dockerfile`).
With `NOAUDIO=0` the image installs the ALSA headers and, because cgo cannot cross-compile here, leaves out
`linux/arm64` unless you name it in `TARGETS`.

**Local audio** uses [oto](https://github.com/ebitengine/oto). It builds without cgo on Windows and macOS. On
Linux it needs cgo and the ALSA headers, so `build.sh` makes the Linux binaries with `-tags noaudio` (UPnP and
proxy modes only, which is what a headless service wants). For local playback on Linux build natively with
`sudo apt install libasound2-dev` and `LINUX_AUDIO=1 hack/build.sh`.

## Format support

LMS only sends a player the formats it announces. GoSlim announces **FLAC, PCM (WAV, AIFF, raw), MP3, AAC, ALAC and
Ogg Vorbis**, all decoded natively in Go with no ffmpeg. Anything else (Opus, WMA, and so on) is **transcoded by LMS**:
it converts the file to FLAC or MP3 on the fly, according to the conversion rules under *Settings > Advanced > File
Types*, and the player never sees the original.

Each of the newer decoders has a switch to hand its format back to LMS: `-nativeaac=false`, `-nativealac=false`,
`-nativevorbis=false` (the format is then not announced and LMS transcodes it).

* **AAC**: AAC-LC in ADTS (`.aac`) or in MP4/M4A, mono or stereo, 8 to 96 kHz. **HE-AAC** (SBR/PS, common in internet
  radio and some low-bitrate files), LATM, ADIF and multichannel are not covered natively. With `-ffmpeg` those are
  handed to ffmpeg; without it the track fails with a message naming the problem.
* **ALAC**: Apple Lossless in MP4/M4A, 16 or 24-bit, mono or stereo, any sample rate. Other layouts (more than two
  channels, 20 or 32-bit) are handed to ffmpeg if `-ffmpeg` is given, otherwise refused.
* **Ogg Vorbis**: any Vorbis stream in Ogg, mono or stereo (multichannel files use their front pair). Ogg Opus and
  FLAC-in-Ogg are not Vorbis and are not decoded.

All decoded audio is turned into 16-bit stereo and resampled to `-rate` (48 kHz by default) with a windowed-sinc
resampler. Deeper samples are truncated to their top 16 bits (no dither), mono is duplicated to both channels, and
for files with more than two channels the first two are used (no downmix). ReplayGain is applied to every format.

### What was measured

Snapshot (October 2026): a synthetic 5 s stereo test signal (two tones plus noise per channel; not a real
recording), generated at every rate and depth with `sox`, `flac` and `ffmpeg`, decoded with the same code the
players use. "Bit-exact" means identical to the source's top 16 bits. Lossy formats are compared with ffmpeg's
decoder, which agrees with LAME's to within 1 LSB on these files. **Speed** is clip duration divided by decode time
(best of 3, one core of a Linux x86-64 container), so 200× decodes 200 s of audio per second; it does not include
resampling. Regenerate it with the commands at the end of this section.

**Lossless, by bit depth** (48 kHz stereo)

| Depth | WAV | AIFF | FLAC |
|---|---|---|---|
| 8-bit | bit-exact, 3742× | bit-exact, 4011× | bit-exact, 278× |
| 16-bit | bit-exact, 2724× | bit-exact, 2985× | bit-exact, 220× |
| 24-bit | bit-exact, 3392× | bit-exact, 2610× | bit-exact, 178× |
| 32-bit | bit-exact, 2034× | bit-exact, 2209× | **not decoded** |
| 32-bit float | **not supported** | not tested | n/a |

The FLAC decoder (`mewkiz/flac`) rejects 32-bit FLAC files ("reserved sample size bit pattern"). 32-bit FLAC is rare,
and such files can be left to LMS to transcode, but it is the one gap in the lossless table.

**Lossless, by sample rate** (24-bit stereo; 16-bit and 32-bit are bit-exact at every rate too, FLAC up to 24-bit)

| Rate | WAV | AIFF | FLAC |
|---|---|---|---|
| 44.1 kHz | bit-exact, 3088× | bit-exact, 2869× | bit-exact, 221× |
| 48 kHz | bit-exact, 3392× | bit-exact, 2610× | bit-exact, 178× |
| 88.2 kHz | bit-exact, 1411× | bit-exact, 1320× | bit-exact, 86× |
| 96 kHz | bit-exact, 1362× | bit-exact, 1334× | bit-exact, 90× |
| 176.4 kHz | bit-exact, 857× | bit-exact, 862× | bit-exact, 58× |
| 192 kHz | bit-exact, 847× | bit-exact, 850× | bit-exact, 61× |

**Channels** (48 kHz): 16-bit mono is bit-exact in WAV, AIFF and FLAC. 24-bit 5.1 (6 channels) is bit-exact on the
front left and right channels in WAV and FLAC.

**MP3** (compared with ffmpeg)

| Stream | Result | Speed |
|---|---|---|
| MPEG-1, 32 / 44.1 / 48 kHz; 128 and 320 kbps CBR, VBR V0 | within 2 LSB (about 80 dB SNR) | 61-105× |
| MPEG-1 mono, 44.1 kHz, 128 kbps | within 2 LSB | 152× |
| MPEG-2, 16 kHz (64 and 160 kbps) | within 2 LSB | 192-218× |
| MPEG-2, 24 kHz, 160 kbps | within 2 LSB | 125× |
| MPEG-2, 24 kHz 64 kbps; 22.05 kHz 64 and 160 kbps | **decoded wrongly** (errors up to full scale) | 148-160× |
| MPEG-2.5, 8 / 11.025 / 12 kHz | **not decoded** | n/a |

The MP3 decoder (`go-mp3`) is fine for ordinary MPEG-1 music files, which is nearly all of them. Some low-rate
MPEG-2 files (22.05 and 24 kHz, typically speech) come out wrong without any error, and MPEG-2.5 is refused. If you
have such files, tell LMS not to send MP3 natively, or convert them.

**AAC, native** (compared with ffmpeg's decoder; the output is in fact bit-identical to its fixed-point `aac_fixed`)

| Input | Result | Speed |
|---|---|---|
| AAC-LC in ADTS (`.aac`), 44.1 / 48 kHz, 128-256 kbps | matches ffmpeg (±1 LSB) | 515-524× |
| AAC-LC in M4A, index (moov) at the start, CBR and VBR | matches ffmpeg | 341-544× |
| AAC-LC in M4A, index at the end | matches ffmpeg | 451× |
| AAC-LC mono in M4A, 44.1 kHz | matches ffmpeg | 854× |
| AAC-LC in M4A at 8, 22.05, 32, 96 kHz | matches ffmpeg | 209-2808× |
| HE-AAC, LATM, ADIF | not decoded natively (ffmpeg if `-ffmpeg` is given) | n/a |

M4A files are played **gapless**: the encoder delay and padding are trimmed using the file's edit list, so a 6 s file
gives exactly 6 s. (ffmpeg itself leaves the end padding in, so the comparison trims its output to the same length.)
When the index comes first the audio is decoded as it arrives; when it comes last the whole file is held in memory
(up to 512 MB) before playback starts, which on a LAN takes a moment.

**ALAC, native** (compared with the source signal)

| Input | Result | Speed |
|---|---|---|
| 16-bit stereo in M4A, index at the start or the end; 44.1 and 48 kHz | bit-exact | 148-178× |
| 24-bit stereo in M4A, 48 / 96 / 192 kHz | bit-exact | 31-112× |
| 16-bit and 24-bit mono, 44.1 / 96 kHz | bit-exact | 126-314× |
| 16-bit, 8 kHz; uncompressed ALAC (`-compression_level 0`) | bit-exact | 669-810× |

M4A files are played gapless in the same way as AAC. **Only files made by ffmpeg's ALAC encoder were available for
testing**, not files written by iTunes or other encoders (the format is the same, but that is untested); the decoder
was also checked against 16 further encoder settings, and with deliberately damaged files, which fail cleanly.

**Ogg Vorbis, native** (compared with ffmpeg's libvorbis decoder; the Ogg decoder is
[jfreymuth/oggvorbis](https://github.com/jfreymuth/oggvorbis))

| Input | Result | Speed |
|---|---|---|
| Stereo 44.1 kHz, q0 / q5 / 128 kbps | matches ffmpeg (±1 LSB) | 241-340× |
| Stereo 48 kHz, q10 (about 500 kbps) | matches ffmpeg (±1 LSB) | 154× |
| Stereo 32 / 22.05 / 8 kHz | matches ffmpeg (±1 LSB) | 306-974× |
| Stereo 96 kHz | matches ffmpeg (±1 LSB) | 140× |
| Mono 44.1 kHz | matches ffmpeg (±1 LSB) | 573× |

**Raw PCM** (no header; LMS describes the format in its command): 8, 16 (both byte orders), 24 and 32-bit, mono and
stereo, bit-exact at 3805× / 2703× / 2796× / 1227× / 8308× / 5640× for the six cases tested.

### Libraries, licences and Go version

* **AAC** is [go-aac](https://github.com/tphakala/go-aac) (a port of FFmpeg's fixed-point AAC-LC decoder), licensed
  **LGPL-2.1-or-later**. Using it unmodified as a dependency is fine for personal use; if you distribute binaries,
  the LGPL's terms (offering the library's source, allowing relinking) apply to them. It requires **Go 1.26 or
  newer**. The MP4 parsing is GoSlim's own, `internal/decode/mp4.go`, because
  [go-m4a](https://github.com/tphakala/go-m4a) wants a seekable source and Go 1.27.
* **ALAC** is [alicebob/alac](https://github.com/alicebob/alac) (MIT), **copied into `internal/alac`** rather than
  fetched, because the original has no `go.mod`, hard-coded stream parameters, and printed errors instead of
  returning them. The copy takes the parameters from the file, validates them, returns errors, rejects oversized
  frames and keeps the MIT licence (`internal/alac/LICENSE.alicebob`). The changes are listed at the top of
  `internal/alac/decode.go`.
* **Ogg Vorbis** is [jfreymuth/oggvorbis](https://github.com/jfreymuth/oggvorbis) and its `vorbis` package (MIT),
  fetched by `go get`.
* MP3 is [go-mp3](https://github.com/hajimehoshi/go-mp3), FLAC is [mewkiz/flac](https://github.com/mewkiz/flac).

Still not used: [go-flac](https://github.com/tphakala/go-flac) (FLAC, bit-exact at 32-bit, which would close the FLAC
gap above).

### Regenerating the table

```
python3 hack/formats/gen.py /tmp/fmt [pcm flac mp3 aac alac ogg]   # needs numpy and ffmpeg; sox and flac are optional
DECODE_MATRIX_DIR=/tmp/fmt FFMPEG=$(which ffmpeg) DECODE_MATRIX_OUT=/tmp/matrix.md \
  go test -run FormatMatrix -v ./internal/decode/
```

`gen.py` makes a test file for every supported format, rate and depth (WAV, AIFF, raw PCM, FLAC, MP3, AAC in ADTS and
M4A, ALAC, Ogg Vorbis, plus 32-bit float WAV, which should be reported as not supported). Without `sox` and `flac` it
uses ffmpeg for AIFF and FLAC, and then leaves out the 8-bit and 32-bit FLAC rows, because ffmpeg's FLAC encoder
silently writes those as 16 and 24-bit; those two rows above were measured with the `flac` tool.

## Proxy mode: the port and mDNS

`goslim -proxyport 3434` waits for `goupnpaudioclient` connections. If port 3434 is already taken (another instance
on the same machine, say), goslim takes any free port instead and **announces it with mDNS** as service
`_goslimproxy._tcp`, named `GoSlim <host name> <port>` (change it with `-mdnsname`). `-proxyport -1` always picks a
free port. `-mdns=false` turns the announcement off, and then a busy port is an error as before.

`goupnpaudioclient` connects to `-host`:`-p` (127.0.0.1:3434 by default). If that fails it looks for a GoSlim with
mDNS for up to 3 seconds and connects to what answers, preferring the one at `-host`; `-mdnsname text` only accepts
a GoSlim whose name contains that text, which tells several apart. The address that worked is tried first on later
reconnects. `-mdns=false` turns the lookup off.

Things to know: mDNS uses UDP port 5353 (multicast 224.0.0.251), so firewalls must allow it, and it normally does not
cross routers or VLANs; there, give `-host` and `-p` explicitly. The lookup only starts when the connection is
*refused or times out*: if another program is listening on 3434 the client connects to that, so a fixed `-p` is still
the way to be certain. IPv4 only.

## Devices

What GoSlim sends a renderer in its default mode is **16-bit stereo PCM in a WAV stream** with no Content-Length (HTTP chunked), at 48 kHz unless
`-rate` says otherwise. It does not use `SetNextAVTransportURI`, and it does not need the renderer to seek. This list comes from the device
profiles of [squeeze2upnp](https://github.com/philippe44/LMS-uPnP) (the bridge GoSlim replaces), which record what each family of renderers
accepts. Only the first row has been run with GoSlim.

| Status | Device (squeeze2upnp profile) | What the profile says | Notes for GoSlim |
|---|---|---|---|
| **Works** | Sonos Play:1, Play:5 | WAV, 48 kHz; Sonos ignores OGG | Tested. 48 kHz default suits it. |
| untested - should work | Other Sonos players | same profile as above | Same family and firmware line as the tested ones. |
| untested - should work | Denon (non-HEOS UPnP) | no codec limit, up to 192 kHz | `-rate 96000` or `192000` if you want hi-res to pass through unresampled. |
| untested - should work | Marantz NR1603, CR603 | PCM/WAV accepted, up to 96 kHz, L24 | CR603's profile lists `pcm,wav` explicitly. Use `-rate 96000` for hi-res. |
| untested - should work | Pioneer N30, XW-SMA | WAV/AIFF/raw accepted, up to 192 kHz | |
| untested - should work | Raumfeld | PCM, FLAC, MP3 | |
| untested - should work | Naim Uniti2 | PCM, FLAC, MP3, AAC, up to 192 kHz | The profile needs a seek after pause. GoSlim does not seek, so check that resuming after a pause plays. |
| untested - should work | M1Clic | PCM, FLAC, MP3, up to 192 kHz | |
| untested - should work | ROCKI | profile asks for a stream with no length | GoSlim's stream has no length by default. |
| untested - should work | Xbox | PCM, MP3; chunked stream | GoSlim's stream is chunked HTTP/1.1. |
| untested - should work | JRiver (Media Center) | PCM among its codecs, no FLAC, chunked stream | |
| untested - should work | Bose SoundTouch | only "no next-URI support" | GoSlim never sends a next URI. Codec support not recorded in the profile. |
| untested - may need `-clen` | Jongo S3x, PURE One Flow | PCM accepted, but the profile forces a fixed stream length of 1000000000 | Try `-clen 1000000000` if playback stops or will not start. |
| untested - may need `-clen` | Roku SoundBridge | PCM/MP3, stream with no length | Old HTTP/1.0 devices may not accept chunked streams; try `-clen`. |
| untested - not PCM | PlugPlayer, Onkyo | FLAC and MP3 only, no PCM | Will probably not play WAV. Use `decodeaudio=false` to pass LMS's FLAC/MP3 through (then no ReplayGain, no resampling). |
| untested - not PCM | Heos (Denon HEOS) | profile forces FLAC, MP3 or AAC MIME types | PCM is not in the list. Try `decodeaudio=false`. |
| untested | foobar2000 (UPnP renderer), Revo, GGMM E5 | profile only forces AAC / turns metadata off | Nothing recorded about PCM support. |

"Should work" is an inference from the profile, not a test: a profile lists what squeeze2upnp had to adjust, and says nothing when a device needed
no adjustment. Reports of working or failing devices are welcome. The squeeze2upnp settings `forced_mimetypes`, `accept_nexturi`,
`seek_after_pause` and the metadata/cover-art switches have no equivalent here (the sample rate is `-rate`, the stream length `-clen`, the codec
choice `decodeaudio`); GoSlim has no per-device profile file.

## Config file (goslimplayer -u)

```json
{
  "server": "192.168.2.20:3483",
  "players": [
    { "name": "FamilyRoom", "mac": "b8:e9:37:28:ae:dc" },
    { "name": "SunL", "mac": "94:9f:3e:8f:63:0c",
      "volume_offset": -20, "volume_max": 70, "buffer_seconds": 15, "http_port": 30001 }
  ]
}
```

Per-player keys (all optional; a missing key uses the matching command-line flag):

| key | values | meaning |
|---|---|---|
| `volume_offset`, `volume_max` | integers | shift / cap the renderer volume |
| `buffer_seconds` | seconds, 0 = unlimited | how far ahead of playback audio is sent |
| `http_port` | port | fixed port for this player's audio stream; must be free at start-up |
| `decodeaudio` | `true` / `false` | `false` passes LMS's original MP3/FLAC/WAV/AIFF/AAC/ALAC/Ogg to the renderer untouched (AAC, ALAC and Ogg pass through too; AAC in M4A with the index at the end may not play on a renderer that cannot seek in a live stream). Least CPU, but no ReplayGain, a short gap between tracks, and the elapsed time is estimated from the clock. Turns resampling off too |
| `resampleaudio` | `true` / `false` | `false` sends each track at its own sample rate; a rate change starts a new stream (short gap) |
| `autoresume` | `true` / `false` | after a Wi-Fi drop or renderer reset, start a new stream and carry on instead of pausing LMS. Pressing stop in LMS still stops |
| `icy` | `true` / `false` | send track title and cover art as ICY in-band metadata to a renderer that asks for it (flag `-icy`, default off; see *ICY metadata* below) |

The defaults are `decodeaudio` and `resampleaudio` on, `autoresume` and `icy` off (flags `-decodeaudio`, `-resampleaudio`,
`-autoresume`, `-icy`). `-exitonnetfail` (default off; on in the systemd unit) makes the program exit with an error when a
renderer never fetches its stream (`renderer never fetched stream … firewall? wrong network?`), so systemd restarts
it every 5 s until the network is right. `-v` (also on `goslim`) logs each track's type, bitrate/VBR and sample rate, the LMS volume
whenever it is sent to the renderer, and per-minute stream statistics.

**Lost streams and autoresume.** Whenever a renderer's stream is lost unexpectedly (it disconnects, or reports STOPPED
without ending the stream), `goslimplayer` keeps a record of the moment: time, reason, stream number, stream age, bytes
appended and served, cumulative bytes sent and consumed, bytes in flight, the last transport state, how long ago the
renderer last pulled audio, track count, whether paused, whether autoresume restarted it and how long the renderer
took to answer again. The last 100 are printed as a `restart summary` when the player closes (Ctrl-C or SIGTERM), so
an overnight run can be reviewed for a pattern. With `-v`, a line `autoresumes so far: N (streams lost: M, last lost
stream at …)` is logged at each track change, and a summary saying nothing was lost is printed at the end if so.

### ICY metadata (track title and cover art)

Some UPnP players understand [ICY in-band metadata](https://gist.github.com/niko/2a1d7b2d109ebe7f7ca2f860c3505ef0), as
used by Icecast/SHOUTcast. It is **off by default**; turn it on with `-icy` or `"icy": true` for a player.

When on, a renderer whose request carries `Icy-Metadata: 1` is answered with `icy-metaint: 16000` (and `icy-name`),
and after every 16000 bytes of the body GoSlim inserts a metadata block holding
`StreamTitle='Artist - Title';StreamUrl='<cover art URL>';`. The title comes from LMS's web interface (the same lookup
that names tracks in the log, so `-webport`, `-webuser` and `-webpass` apply) and the cover is LMS's own
`http://<LMS>:<webport>/music/<id>/cover.jpg`, or the artwork URL LMS holds for a radio stream. A block is sent only
when the track changes; the other blocks are the single zero byte that means "no change". The metadata follows the
audio: it switches when the renderer reaches the new track, and a replacement stream (after a rotation or an
autoresume) starts with the current track's.

Things to know:
* It works on every stream type, but it is meant for MP3/AAC. With decoded WAV (the default) a renderer that sends
  `Icy-Metadata: 1` yet does not apply it to a WAV stream would play the metadata as a click every 16000 bytes. That is
  why it is off by default; try it with your players, and prefer `"decodeaudio": false` for the ones that need it.
* It is not used when `-clen` gives the stream a fixed Content-Length (the length would be wrong).
* The title comes from LMS about 0.4 s after a track starts. A block that needs it waits up to 1.5 s for the answer
  (only the first block of a fresh stream can ever stall, and only when the renderer asks before LMS has answered).
* Only `StreamTitle` and `StreamUrl` are sent. The UPnP metadata given to the renderer (DIDL-Lite) is unchanged.

`goslimplayer -jsonfile` lists the renderers and writes a skeleton. Every entry needs the `name` key (it may be
empty to use the renderer's own name) and a `mac`. `server` is optional; without it, and without `-server`, LMS
is found by broadcast once and shared by all players.

## Running as a service

The programs stop cleanly on SIGINT/SIGTERM: they leave LMS, stop the speakers and exit within seconds. That is
what systemd sends, and what NSSM sends a console program, so no service-specific code is needed.

* **Linux**: `deploy/goslimplayer.service` (steps in its header). The hardening section allows `AF_NETLINK`,
  which SSDP discovery needs; if discovery still fails, add `AF_PACKET` to `RestrictAddressFamilies`. Logs go to `journalctl -u goslimplayer`.
* **Windows**: edit `INSTALLDIR` in `deploy/install-nssm.bat` and run it as administrator.

Use UPnP mode for services. A Windows service runs in session 0 and may not reach the sound card, so local
playback is best run as a normal program there.

### Long playback, ports and recovery

* **4 GiB limit.** The stream is a WAV, whose length field is 32 bits, so a renderer stops after about 4 GiB
  (roughly 6.5 hours of 44.1 kHz stereo; the log shows `renderer stream ended after 4304994304 bytes`). The player
  now starts a fresh stream at the next track change once 3 GiB have been sent (forced after a further 256 MiB)
  and switches over only after the renderer has played the old one out. This is experimental; `-v` logs each switch.
* **Ports.** An explicit `http_port` must be free or the program exits with a clear message. With
  `-httpstartport` a taken port is skipped (up to 50 are tried), otherwise the OS chooses. If the HTTP listener
  fails later it is re-opened on the same port, or the next free one.
* **Idle CPU.** Waiting is event driven, so an idle player uses well under 0.1% of a core.

## Testing

```
go test ./...                       # unit tests, including a fake-renderer end-to-end test of goslimplayer's output
python3 hack/itest.py ./goslim      # end-to-end test of proxy mode against a fake LMS (see the file's header)
ITEST_FORMAT=alac python3 hack/itest.py ./goslim   # the same with wav (default), aac, alac or ogg tracks (needs ffmpeg)
```
