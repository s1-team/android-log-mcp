//go:build !unix && !windows

package service

import "os/exec"

func detach(cmd *exec.Cmd) error { return cmd.Start() }
