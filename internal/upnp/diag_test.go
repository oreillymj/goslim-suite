package upnp

import (
	"io"
	"os"
	"strings"
	"testing"
)

func capture(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestPrintMemberMarksSelfAndCoordinator(t *testing.T) {
	coord := zgMember{UUID: "RINCON_949F3E8F630C01400", ZoneName: "SunRoomL", Location: "http://127.0.0.1:1/xml/device_description.xml"}
	other := zgMember{UUID: "RINCON_7828CAA2808C01400", ZoneName: "SunRoomL", Location: "http://127.0.0.1:1/xml/device_description.xml", Invisible: "1"}
	self := "uuid:RINCON_7828CAA2808C01400_MR"

	out := capture(func() { printMember(other, "", self, coord.UUID); printMember(coord, "", self, coord.UUID) })
	if !strings.Contains(out, "RINCON_7828CAA2808C01400   <-- this renderer\n") || !strings.Contains(out, "RINCON_949F3E8F630C01400   <-- coordinating renderer\n") {
		t.Errorf("tags wrong:\n%s", out)
	}
	if !strings.Contains(out, "MAC: 78:28:ca:a2:80:8c") || !strings.Contains(out, "MAC: 94:9f:3e:8f:63:0c") {
		t.Errorf("MACs missing:\n%s", out)
	}
	out = capture(func() { printMember(coord, "", "uuid:RINCON_949F3E8F630C01400_MR", coord.UUID) })
	if !strings.Contains(out, "<-- this renderer, coordinator") {
		t.Errorf("self+coordinator tag wrong:\n%s", out)
	}
}
