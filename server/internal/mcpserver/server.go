// Package mcpserver exposes the four read-only logcat tools over MCP.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/device"
)

// Version of the server.
var Version = "0.2.1" // overridden at build time: -ldflags "-X main.version=..."

const instructions = `Android logcat recorder. Logs from every connected device are captured continuously in the background and kept across sessions; nothing to start/stop/clear.
Typical flows: crashes/ANRs → logcat_crashes. Startup time → logcat_app_launches. Anything else → logcat_search (use view="summary" first for wide ranges).
To capture a reproduction: logcat_status → take ` + "`cursor`" + ` → user reproduces → logcat_search(after=cursor, wait_ms=20000).
All times are device-local; responses state the device timezone.`

// Tools implements the tool logic over a device manager. It is the seam for a
// future shared daemon: a thin proxy could forward (name, raw args) to a
// daemon process running the same Tools and relay the text back.
type Tools struct {
	M *device.Manager
}

// toolError is a business error returned with isError=true.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func errf(format string, a ...any) error { return &toolError{fmt.Sprintf(format, a...)} }

// Call dispatches a tool by name with raw JSON args, returning text and isError.
func (t *Tools) Call(ctx context.Context, name string, raw json.RawMessage) (string, bool) {
	var (
		out string
		err error
	)
	switch name {
	case "logcat_status":
		var a statusArgs
		if err = decodeArgs(raw, &a); err == nil {
			out, err = t.status(ctx, a)
		}
	case "logcat_search":
		var a searchArgs
		if err = decodeArgs(raw, &a); err == nil {
			out, err = t.search(ctx, a)
		}
	case "logcat_crashes":
		var a crashArgs
		if err = decodeArgs(raw, &a); err == nil {
			out, err = t.crashes(ctx, a)
		}
	case "logcat_app_launches":
		var a launchArgs
		if err = decodeArgs(raw, &a); err == nil {
			out, err = t.launches(ctx, a)
		}
	default:
		err = errf("unknown tool %q", name)
	}
	if err != nil {
		return err.Error(), true
	}
	return out, false
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			return errf("unknown parameter %s. Valid parameters: %s", strings.TrimPrefix(msg, "json: unknown field "), strings.Join(fieldNames(v), ", "))
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return errf("parameter %q has the wrong type: expected %s", te.Field, te.Type.String())
		}
		return errf("invalid arguments: %v", err)
	}
	return nil
}

func fieldNames(v any) []string {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func boolPtr(b bool) *bool { return &b }

// NewServer builds the MCP server with the 4 tools.
func NewServer(t *Tools) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "android-log", Title: "Android logcat", Version: Version}, &mcp.ServerOptions{Instructions: instructions})
	for _, def := range toolDefs() {
		def := def
		s.AddTool(&mcp.Tool{
			Name:        def.name,
			Title:       def.title,
			Description: def.desc,
			InputSchema: json.RawMessage(def.schema),
			Annotations: &mcp.ToolAnnotations{Title: def.title, ReadOnlyHint: true, OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(false), IdempotentHint: true},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var raw json.RawMessage
			if req.Params != nil {
				raw = req.Params.Arguments
			}
			text, isErr := t.Call(ctx, def.name, raw)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}, nil
		})
	}
	return s
}
