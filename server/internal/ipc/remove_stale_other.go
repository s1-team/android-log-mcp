//go:build !windows

package ipc

import "os"

func removeStaleSocket(path string) {
	_ = os.Remove(path)
}
