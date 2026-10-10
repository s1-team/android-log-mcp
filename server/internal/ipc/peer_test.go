//go:build linux || darwin

package ipc

import (
	"os"
	"testing"
)

func TestSamePeer(t *testing.T) {
	if samePeer(os.Getuid()) != nil {
		t.Fatal("own uid rejected")
	}
	if samePeer(os.Getuid()+1) == nil {
		t.Fatal("other uid accepted")
	}
}
