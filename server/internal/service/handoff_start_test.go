package service

// Issue #109: the first logcat_status after a version handoff (or the
// replacement of a hung service process) starts the named device even when
// the new service process reads its identity slowly.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
)

func TestFirstStatusAfterHandoffStartsDevice(t *testing.T) {
	e := installFake(t, "FAKEA")
	ctrl := testControl(t)
	old := startTest(t, ctrl, "0.4.0")
	c, err := Dial(context.Background(), ctrl, "0.4.0", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	var r CallReply
	args := []byte(`{"device":"FAKEA"}`)
	if err := c.Do(context.Background(), "call", CallArgs{Name: "logcat_status", Args: args}, &r); err != nil || r.IsError {
		t.Fatalf("first service: %v %s", err, r.Text)
	}
	c.Close()

	// the service process is replaced (a hung one is killed and replaced, a
	// handoff reaches the same state: the new one starts without the devices
	// that calls had started); it finds the device slow
	old.Stop("replaced")
	<-old.Done()
	e.SetIdentDelay("FAKEA", 3*time.Second)
	prev := device.CatchUpWait
	device.CatchUpWait = 300 * time.Millisecond
	t.Cleanup(func() { device.CatchUpWait = prev })
	s := startTest(t, ctrl, "0.4.1")
	c2, err := Dial(context.Background(), ctrl, "0.4.1", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	r = CallReply{}
	if err := c2.Do(context.Background(), "call", CallArgs{Name: "logcat_status", Args: args}, &r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "recording: idle") || strings.Contains(r.Text, "not recording") {
		t.Fatalf("first status after the handoff:\n%s", r.Text)
	}
	if !s.Manager().Wanted("FAKEA") {
		t.Fatalf("device not started:\n%s", r.Text)
	}
}
