//go:build !unix && !windows

package device

import "os/exec"

func tieToParent(*exec.Cmd) {}
func ownGroup(*exec.Cmd)    {}
