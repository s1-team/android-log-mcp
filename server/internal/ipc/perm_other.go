//go:build !unix && !windows

package ipc

import "os"

func restrictDir(dir string) error  { return os.Chmod(dir, 0o700) }
func restrictSocket(p string) error { return os.Chmod(p, 0o600) }
func unsupported(err error) bool    { return false }
