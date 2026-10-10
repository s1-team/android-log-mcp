package service

import (
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func lockHeld(p string) bool {
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return store.LockHeld(p)
}
