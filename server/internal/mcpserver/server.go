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
var Version = "0.5.2" // overridden at build time: -ldflags "-X main.version=..."

const instructions = `Android logcat recorder. Recording starts on use: the first log tool call for a device starts recording it and backfills what is still in the device's log buffer (the call waits up to 10 s for that). Each device stops recording after a while without calls; stored logs stay queryable across sessions.
Typical flows: crashes/ANRs → logcat_crashes. Startup time → logcat_app_launches. Anything else → logcat_search (use view="summary" first for wide ranges).
To capture a reproduction: logcat_status → take ` + "`cursor`" + ` → user reproduces → logcat_search(after=cursor, wait_ms=20000). If logcat_status says "backfill in progress", call logcat_status again before using a cursor.
To keep recording without calls (e.g. a long reproduction), call logcat_status(record_for="30m"); record_for="0" stops.
All times are device-local; responses state the device timezone.`

// Tools implements the tool logic over a device manager. It runs in the
// background service; MCP processes are thin clients that forward
// (name, raw args) to it and relay the text back (issue #3).
type Tools struct {
	M *device.Manager
	// Service, if set, describes the service process (pid, version,
	// protocol, root …); logcat_status prints it first.
	Service func() []StatusLine
	// Pins, if set, implements record_for (the service's pins).
	Pins Pins
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

// ToolDef is a tool definition as sent from the service to its clients
// (hello), so a client always lists exactly the tools the service runs.
type ToolDef struct {
	Name   string          `json:"name"`
	Title  string          `json:"title"`
	Desc   string          `json:"desc"`
	Schema json.RawMessage `json:"schema"`
}

// ToolDefs are this binary's tool definitions.
func ToolDefs() []ToolDef {
	var out []ToolDef
	for _, d := range toolDefs() {
		out = append(out, ToolDef{Name: d.name, Title: d.title, Desc: d.desc, Schema: json.RawMessage(d.schema)})
	}
	return out
}

// Instructions are this binary's MCP server instructions.
func Instructions() string { return instructions }

// CallFunc runs one tool: (name, raw JSON args) → (text, isError).
type CallFunc func(ctx context.Context, name string, raw json.RawMessage) (string, bool)

// NewServer builds the MCP server with the 4 tools, run in this process.
func NewServer(t *Tools) *mcp.Server {
	return NewProxyServer(ToolDefs(), Instructions(), t.Call)
}

// NewProxyServer builds an MCP server from tool definitions whose calls go
// through call (the thin client forwards them to the background service).
func NewProxyServer(defs []ToolDef, instr string, call CallFunc) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "android-log", Title: "Android logcat", Version: Version}, &mcp.ServerOptions{Instructions: instr})
	for _, def := range defs {
		def := def
		s.AddTool(&mcp.Tool{
			Name:        def.Name,
			Title:       def.Title,
			Description: def.Desc,
			InputSchema: def.Schema,
			Annotations: &mcp.ToolAnnotations{Title: def.Title, ReadOnlyHint: true, OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(false), IdempotentHint: true},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var raw json.RawMessage
			if req.Params != nil {
				raw = req.Params.Arguments
			}
			text, isErr := call(ctx, def.Name, raw)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}, nil
		})
	}
	return s
}
