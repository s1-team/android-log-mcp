package layout

import (
	"errors"
	"os"
)

// RemovedHomeEnv is the environment variable that used to relocate the data
// root (removed in 0.4.0, issues #9, #10, #3). The root is DefaultDir() unless
// `android-log-mcp set-root` moved it (for every client, via the service).
const RemovedHomeEnv = "ANDROID_LOG_MCP_HOME"

// ErrHomeRemoved explains the removal of ANDROID_LOG_MCP_HOME / --home.
var ErrHomeRemoved = errors.New(RemovedHomeEnv + " and --home were removed in 0.4.0: the data root is " +
	"<user cache dir>/android-log-mcp by default (see `android-log-mcp root`) and is moved for every MCP and CLI process with " +
	"`android-log-mcp set-root <dir> --keep-old|--delete-old`. Unset " + RemovedHomeEnv +
	" / drop --home from your MCP host configuration")

// CheckRemovedHome returns ErrHomeRemoved when the removed env var is set.
func CheckRemovedHome() error {
	if os.Getenv(RemovedHomeEnv) != "" {
		return ErrHomeRemoved
	}
	return nil
}
