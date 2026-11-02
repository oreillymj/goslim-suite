// Package about lists the open source projects built into the GoSlim programs and their licenses
// (the -about and -licenses flags). The license texts are copies of the files shipped with each module,
// embedded in the binary; go.mod and the LICENSE files they were copied from are the source of truth, so
// update this table and licenses/ when a dependency is added or upgraded (TestEveryModuleListed checks it).
package about

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"
)

//go:embed licenses/*.txt
var texts embed.FS

// Project is one open source component.
type Project struct {
	Module  string   // Go module path, as in go.mod ("" for code vendored into this repository)
	Name    string   // short display name
	URL     string   // project home
	License string   // SPDX identifier(s)
	Used    string   // what GoSlim uses it for
	Files   []string // embedded license texts (licenses/<name>.txt)
}

// Projects is every third-party component that can be linked into a GoSlim binary.
var Projects = []Project{
	{"", "Go standard library", "https://go.dev", "BSD-3-Clause", "the language runtime and standard library", []string{"go-stdlib"}},
	{"golang.org/x/net", "Go x/net", "https://github.com/golang/net", "BSD-3-Clause", "multicast sockets for mDNS", []string{"golang-bsd3", "golang-patents"}},
	{"golang.org/x/sys", "Go x/sys", "https://github.com/golang/sys", "BSD-3-Clause", "operating system calls", []string{"golang-bsd3", "golang-patents"}},
	{"github.com/hajimehoshi/go-mp3", "go-mp3", "https://github.com/hajimehoshi/go-mp3", "Apache-2.0", "MP3 decoder", []string{"go-mp3"}},
	{"github.com/mewkiz/flac", "mewkiz/flac", "https://github.com/mewkiz/flac", "Unlicense", "FLAC decoder", []string{"unlicense"}},
	{"github.com/mewkiz/pkg", "mewkiz/pkg", "https://github.com/mewkiz/pkg", "Unlicense", "helpers for the FLAC decoder", []string{"unlicense"}},
	{"github.com/mewpkg/term", "mewpkg/term", "https://github.com/mewpkg/term", "Unlicense", "helpers for the FLAC decoder", []string{"unlicense"}},
	{"github.com/icza/bitio", "icza/bitio", "https://github.com/icza/bitio", "Apache-2.0", "bit reader for the FLAC decoder", []string{"bitio"}},
	{"github.com/jfreymuth/oggvorbis", "oggvorbis", "https://github.com/jfreymuth/oggvorbis", "MIT", "Ogg Vorbis decoder", []string{"oggvorbis"}},
	{"github.com/jfreymuth/vorbis", "vorbis", "https://github.com/jfreymuth/vorbis", "MIT", "Vorbis decoder core", []string{"vorbis"}},
	{"github.com/tphakala/go-aac", "go-aac", "https://github.com/tphakala/go-aac", "LGPL-2.1 (with BSD-3-Clause parts)", "AAC decoder", []string{"go-aac-lgpl", "golang-bsd3"}},
	{"github.com/tphakala/simd", "tphakala/simd", "https://github.com/tphakala/simd", "MIT", "vector maths for the AAC decoder", []string{"simd"}},
	{"github.com/ebitengine/oto/v3", "Oto", "https://github.com/ebitengine/oto", "Apache-2.0", "local audio output (not in -tags noaudio builds)", []string{"oto"}},
	{"github.com/ebitengine/purego", "purego", "https://github.com/ebitengine/purego", "Apache-2.0", "calls system audio libraries without cgo (not in -tags noaudio builds)", []string{"purego"}},
	{"github.com/jfreymuth/pulse", "pulse", "https://github.com/jfreymuth/pulse", "MIT", "PulseAudio client (not in -tags noaudio builds)", []string{"pulse"}},
	{"", "alicebob/alac", "https://github.com/alicebob/alac", "MIT", "ALAC decoder, vendored and modified in internal/alac", []string{"alicebob-alac"}},
	{"", "ALAC decoder by David Hammerton", "http://crazney.net/programs/itunes/alac.html", "MIT", "the original C decoder that alicebob/alac translates", []string{"hammerton-alac"}},
}

// linked reports which modules are in this binary, from the build information the Go
// toolchain embeds. It returns nil when there is none (then everything is listed).
func linked() map[string]string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || len(bi.Deps) == 0 {
		return nil
	}
	m := map[string]string{}
	for _, d := range bi.Deps {
		v := d.Version
		if d.Replace != nil && d.Replace.Version != "" {
			v = d.Replace.Version
		}
		m[d.Path] = v
	}
	return m
}

// Write prints the project list; with full it follows with the license texts, each distinct text once.
func Write(w io.Writer, program, version string, full bool) {
	in := linked()
	fmt.Fprintf(w, "%s %s\n\nThis program includes the following open source software:\n\n", program, version)
	var shown []Project
	for _, p := range Projects {
		v := ""
		if p.Module != "" && in != nil {
			var ok bool
			if v, ok = in[p.Module]; !ok {
				continue // not linked into this build
			}
		}
		shown = append(shown, p)
		name := p.Name
		if v != "" {
			name += " " + v
		}
		fmt.Fprintf(w, "%s\n    License: %s\n    %s\n    Used for: %s\n\n", name, p.License, p.URL, p.Used)
	}
	fmt.Fprintln(w, "Run with -licenses for the full license texts.")
	if !full {
		return
	}
	seen := map[[32]byte][]string{}
	var order [][32]byte
	content := map[[32]byte]string{}
	for _, p := range shown {
		for _, f := range p.Files {
			b, err := texts.ReadFile("licenses/" + f + ".txt")
			if err != nil {
				continue
			}
			h := sha256.Sum256(b)
			if _, ok := seen[h]; !ok {
				order = append(order, h)
				content[h] = string(b)
			}
			seen[h] = append(seen[h], p.Name)
		}
	}
	for _, h := range order {
		names := dedupe(seen[h])
		fmt.Fprintf(w, "\n%s\nLicense text for: %s\n%s\n\n%s\n", strings.Repeat("=", 78), strings.Join(names, ", "), strings.Repeat("=", 78), strings.TrimRight(content[h], "\n"))
	}
}

func dedupe(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
