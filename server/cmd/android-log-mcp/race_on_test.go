//go:build race

package main

// raceEnabled: the race detector slows process starts and adb calls.
const raceEnabled = true
