package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReportHintEnv switches on the reporting hint of connect errors (#83). It
// is read by the MCP / CLI process only, where the error text is built; the
// service never reads it. Off by default. It changes nothing but the text of
// an error: no file is written, nothing is collected and nothing is sent
// (this tool has no network code at all).
const ReportHintEnv = "ANDROID_LOG_MCP_REPORT_HINT"

// ReportHintOn reports whether the value of ReportHintEnv turns the hint on
// (1 / true / yes / on).
func ReportHintOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// suggestedReportPath is where the hint proposes to write the zip: the
// user's Downloads folder (else the home directory), outside the data root so
// retention cannot delete it.
func suggestedReportPath(now time.Time) string {
	dir, _ := os.UserHomeDir()
	if dir != "" {
		if d := filepath.Join(dir, "Downloads"); isDir(d) {
			dir = d
		}
	}
	return filepath.Join(dir, "android-log-mcp-report-"+now.Format("20060102-150405")+".zip")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ReportHintBlock is the text appended to an error when the hint is on: it
// tells the agent to ask the user first and never to send anything itself.
func ReportHintBlock(root, zipPath string) string {
	return fmt.Sprintf(`

Developer reporting is on (%s). This looks like a bug in android-log-mcp. Ask the user first whether you may help them report it, and do not run anything before they say yes. If they agree:
1. Run: android-log-mcp --offline "%s" report --logs-only --out "%s"
   It works while the service is down. It packs the service log, service.json, a diagnosis of the lock and "adb devices -l", with tokens, emails, phone numbers, IP addresses and similar values redacted, into one zip. It sends nothing anywhere.
2. Tell the user the zip's path and ask them to open it and check what it contains.
3. Give the user this link and the text of the error above to paste: https://github.com/s1crab/android-log-mcp/issues/new
   The user attaches the zip and submits the issue. Never upload or send the file yourself.`, ReportHintEnv, root, zipPath)
}
