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

// logcat_status names the process that owns a device recorded elsewhere
// (e.g. another ANDROID_LOG_MCP_HOME) instead of claiming to record it.
func TestStatusNamesForeignOwner(t *testing.T) {
	root, homeB := t.TempDir(), t.TempDir()
	id := device.Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554"}
	m1 := device.NewManager(root, store.Config{IdleRelease: -1})
	m1.Root, m1.Version, m1.NoADB = layout.Root{Dir: root}, "9.9.9", true
	m1.Start(context.Background())
	defer m1.Close()
	m1.Attach(id)

	m2 := device.NewManager(homeB, store.Config{IdleRelease: -1})
	m2.Root, m2.NoADB = layout.Root{Dir: root}, true
	m2.Start(context.Background())
	defer m2.Close()
	m2.Attach(id)

	out, isErr := (&Tools{M: m2}).Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"emulator-5554"}`))
	if isErr {
		t.Fatal(out)
	}
	want := fmt.Sprintf("owned by another android-log-mcp process (pid %d, version 9.9.9, data %s)", os.Getpid(), layout.DeviceDir(root, "avd-a"))
	if !strings.Contains(out, "mode: read-only: ANDROID_LOG_MCP_HOME/--home is set") {
		t.Fatalf("status does not say the HOME process is read-only:\n%s", out)
	}
	if !strings.Contains(out, "NOT recorded by this process") || !strings.Contains(out, want) {
		t.Fatalf("status does not name the owner:\n%s", out)
	}
	out1, _ := (&Tools{M: m1}).Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"avd-a"}`))
	if strings.Contains(out1, "NOT recorded") {
		t.Fatalf("owner's own status:\n%s", out1)
	}
}
