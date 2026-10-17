package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
)

// ExportArgs is the "export" op (`android-log-mcp export`, issue #172): the
// logcat_export tool arguments as JSON plus the CLI's destination. Out must
// be absolute (the service has its own working directory).
type ExportArgs struct {
	Args  json.RawMessage `json:"args"`
	Out   string          `json:"out"`
	Force bool            `json:"force,omitempty"`

	KeepPrivateIP bool     `json:"keep_private_ip,omitempty"`
	KeepDomains   []string `json:"keep_domains,omitempty"`
}

// ExportReply is the "export" op's reply: the tool's text.
type ExportReply struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

func init() {
	extraOps["export"] = func(ctx context.Context, s *Service, h *Hello, body json.RawMessage) (any, error) {
		var a ExportArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		s.mu.Lock()
		t, root := s.tools, s.root
		s.mu.Unlock()
		if t == nil {
			return nil, errors.New("the service has no data root yet")
		}
		text, isErr := t.ExportToFile(mcpserver.WithStatusMemo(ctx, h.status), a.Args, mcpserver.ExportTarget{
			Out: a.Out, Force: a.Force, Protect: []string{root, s.opt.Control.Dir},
			KeepPrivateIP: a.KeepPrivateIP, KeepDomains: a.KeepDomains,
		})
		return ExportReply{Text: text, IsError: isErr}, nil
	}
}

// Export runs the export through the service, starting it detached when none
// runs (like Report). A dropped connection is not retried: a retry after a
// lost reply could write the file twice.
func Export(ctx context.Context, ctrl Control, version string, a ExportArgs) (*ExportReply, error) {
	cl := NewClient(ClientOptions{Control: ctrl, Version: version, Role: "export", NoEmbedded: true})
	defer cl.Close()
	var r ExportReply
	if err := cl.do(ctx, "export", a, &r, false); err != nil {
		var re *ipc.RemoteError
		if errors.As(err, &re) {
			return nil, errors.New(re.Msg)
		}
		return nil, err
	}
	return &r, nil
}
