package layout

import (
	"errors"
	"os"
)

// RemovedHomeEnv is the environment variable that used to relocate the data
// root (removed in 0.4.0, issues #9, #10). The root is fixed to DefaultDir();
// a configurable root (set-root) is planned for the background service, #3.
const RemovedHomeEnv = "ANDROID_LOG_MCP_HOME"

// ErrHomeRemoved explains the removal of ANDROID_LOG_MCP_HOME / --home.
var ErrHomeRemoved = errors.New(RemovedHomeEnv + " and --home were removed in 0.4.0: the data root is now fixed to " +
	"<user cache dir>/android-log-mcp (see `android-log-mcp root`); configurable root switching is planned with the " +
	"background service (https://github.com/s1crab/android-log-mcp/issues/3). Unset " + RemovedHomeEnv +
	" / drop --home from your MCP client configuration")

// CheckRemovedHome returns ErrHomeRemoved when the removed env var is set.
func CheckRemovedHome() error {
	if os.Getenv(RemovedHomeEnv) != "" {
		return ErrHomeRemoved
	}
	return nil
}
