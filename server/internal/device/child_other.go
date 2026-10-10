//go:build !unix

package device

import "os/exec"

func tieToParent(*exec.Cmd) {}
func ownGroup(*exec.Cmd)    {}
