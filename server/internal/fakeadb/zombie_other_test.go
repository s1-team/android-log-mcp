//go:build !linux && !darwin

package fakeadb

func zombie(int) bool { return true }
