package about

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLicenseFilesEmbedded(t *testing.T) {
	for _, p := range Projects {
		if len(p.Files) == 0 {
			t.Errorf("%s: no license text", p.Name)
		}
		for _, f := range p.Files {
			if b, err := texts.ReadFile("licenses/" + f + ".txt"); err != nil || len(b) < 100 {
				t.Errorf("%s: license text %q missing or empty", p.Name, f)
			}
		}
	}
}

// Every module that a GoSlim program links must be in Projects, so a new dependency cannot
// ship without a license entry.
func TestEveryModuleListed(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool")
	}
	listed := map[string]bool{}
	for _, p := range Projects {
		listed[p.Module] = true
	}
	for _, tags := range []string{"", "noaudio"} {
		cmd := exec.Command("go", "list", "-tags", tags, "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", "../../cmd/...")
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("go list: %v", err)
		}
		for _, m := range strings.Fields(string(out)) {
			if !listed[m] {
				t.Errorf("module %s (tags %q) is linked but has no entry in about.Projects", m, tags)
			}
		}
	}
}

func TestWrite(t *testing.T) {
	var b bytes.Buffer
	Write(&b, "prog", "1.0", true)
	for _, want := range []string{"prog 1.0", "alicebob/alac", "License text for:", "Permission is hereby granted"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
