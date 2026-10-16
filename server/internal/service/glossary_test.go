package service

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

// oldWording is what docs/glossary.md retired (#40): output that an agent or
// a user reads must say MCP process / CLI process / MCP host / service process.
var oldWording = regexp.MustCompile(`(?i)\bclients?\b|MCP client|background service`)

func TestUserFacingTextUsesGlossaryTerms(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.4.0")
	c, err := Dial(context.Background(), ctrl, "0.4.0", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var info Info
	if err := c.Do(context.Background(), "status", nil, &info); err != nil {
		t.Fatal(err)
	}
	embedded := info
	embedded.Embedded = "inside MCP process pid 1: test"
	embedded.Lifetime = (&Service{opt: Options{Embedded: "x"}}).lifeText()
	texts := map[string]string{
		"status text":          info.Text(),
		"embedded status text": embedded.Text(),
		"busy reason":          s.busyReason(time.Now()),
		"incompatible error": (&ErrIncompatible{Client: HelloReply{Version: "0.3.0", Proto: 1},
			Service: HelloReply{Version: "0.4.0", Proto: 2, MinProto: 2}}).Error(),
		"--home error": layout.ErrHomeRemoved.Error(),
	}
	for name, txt := range texts {
		if m := oldWording.FindString(txt); m != "" {
			t.Errorf("%s uses the old word %q:\n%s", name, m, txt)
		}
	}
}
