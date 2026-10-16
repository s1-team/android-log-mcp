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
var Version = "0.8.1" // overridden at build time: -ldflags "-X main.version=..."

const instructions = `Android logcat recorder. Recording starts on use: the first log tool call for a device starts recording it and backfills what is still in the device's log buffer (the call waits up to 10 s for that). Each device stops recording after a while without calls; stored logs stay queryable across sessions.
Typical flows: crashes/ANRs → logcat_crashes. Startup time → logcat_app_launches. Anything else → logcat_search (use view="summary" first for wide ranges).
To capture a reproduction: logcat_status → take ` + "`cursor`" + ` → user reproduces → logcat_search(after=cursor, wait_ms=20000). If logcat_status says "backfill in progress", call logcat_status again before using a cursor.
To keep recording without calls (e.g. a long reproduction), call logcat_status(record_for="30m"); record_for="0" stops.
All times in parameters and lines are device time (the Android device's clock, in its timezone); responses show device now, host now and the clock offset, and mark device clock jumps.`

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

// toolError is a business error returned with isError=true. Its text is the
// P3 shape of #85: "error code=<code> k=v ... next: param=value". code is a
// closed enum (docs/reference.md); next is the only part that must be
// executable.
type toolError struct {
	code string
	kv   []any
	next []string
	msg  string // legacy free text (code "failed")
}

func (e *toolError) Error() string {
	if e.code == "" {
		return hdr("error", "code", "failed", "msg", e.msg)
	}
	return hdr("error", append([]any{"code", e.code}, e.kv...)...) + nextClause(e.next...)
}

// errf is an error without a code of its own: code=failed with the message.
// Prefer errc, which carries an executable next step.
func errf(format string, a ...any) error { return &toolError{msg: fmt.Sprintf(format, a...)} }

// errc is a coded error: code, key=value details and executable next steps.
func errc(code string, next []string, kv ...any) error {
	return &toolError{code: code, kv: kv, next: next}
}

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
		var names []string
		for _, d := range toolDefs() {
			names = append(names, d.name)
		}
		err = errc("unknown_tool", []string{"one of " + strings.Join(names, " ")}, "value", name)
	}
	if err != nil {
		var te *toolError
		if !errors.As(err, &te) { // not coded: keep the first token
			return hdr("error", "code", "failed", "msg", err.Error()), true
		}
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
			bad := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
			return errc("unknown_param", []string{"drop " + bad}, "param", bad, "valid", strings.Join(fieldNames(v), ","))
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return errc("bad_type", nil, "param", te.Field, "expected", te.Type.String())
		}
		return errc("bad_json", []string{`pass a JSON object, e.g. {"device":"emulator-5554"}`}, "msg", err.Error())
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
