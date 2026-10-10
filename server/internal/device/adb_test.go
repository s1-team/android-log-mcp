package device

import (
	"bytes"
	"testing"
)

func TestParseDevices(t *testing.T) {
	d := ParseDevices("List of devices attached\nemulator-5554\tdevice\nR5CT123\tunauthorized\nX\tno permissions (user in plugdev group)\n\n")
	if len(d) != 3 || d[0].State != "device" || d[1].State != "unauthorized" || d[2].State != "no permissions" {
		t.Fatalf("%+v", d)
	}
}

func TestTrackFrames(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("0015emulator-5554\tdevice\n")
	buf.WriteString("0000")
	var got [][]DevState
	ReadTrackFrames(&buf, func(d []DevState) { got = append(got, d) })
	if len(got) != 2 || len(got[0]) != 1 || got[0][0].Serial != "emulator-5554" || len(got[1]) != 0 {
		t.Fatalf("%+v", got)
	}
}
