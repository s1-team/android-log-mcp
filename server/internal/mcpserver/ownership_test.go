package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// logcat_status of a process with another data root (only in tests since #10)
// names the process that owns a device instead of claiming to record it;
// owner locks live in one owners dir shared by all roots.
func TestStatusNamesForeignOwner(t *testing.T) {
	root, homeB, owners := t.TempDir(), t.TempDir(), t.TempDir()
	id := device.Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554"}
	start := func(base, ver string) *device.Manager {
		m := device.NewManager(base, store.Config{IdleRelease: -1})
		m.Root, m.Version, m.NoADB = layout.Root{Dir: base, OwnersDir: owners}, ver, true
		m.Start(context.Background()) // no adb in tests
		m.NoADB = false               // ...but attach as a recording process would
		t.Cleanup(m.Close)
		m.Attach(id)
		return m
	}
	m1 := start(root, "9.9.9")
	m2 := start(homeB, "test")

	out, isErr := (&Tools{M: m2}).Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"emulator-5554"}`))
	if isErr {
		t.Fatal(out)
	}
	want := fmt.Sprintf("owned by another android-log-mcp process (pid %d, version 9.9.9, data %s)", os.Getpid(), layout.DeviceDir(root, "avd-a"))
	if !strings.Contains(out, "NOT recorded by this process") || !strings.Contains(out, want) {
		t.Fatalf("status does not name the owner:\n%s", out)
	}
	if !strings.Contains(out, "owner locks: "+owners) {
		t.Fatalf("status mode lines:\n%s", out)
	}
	out1, _ := (&Tools{M: m1}).Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"avd-a"}`))
	if strings.Contains(out1, "NOT recorded") {
		t.Fatalf("owner's own status:\n%s", out1)
	}
}
