//go:build !windows

package ipc

import "testing"

func checkUserOnlyACL(*testing.T, string) {}
