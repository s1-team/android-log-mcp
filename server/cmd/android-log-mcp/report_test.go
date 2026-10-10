package main

import (
	"reflect"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"50MB": 50 << 20, "50m": 50 << 20, "512KiB": 512 << 10, "1.5G": 3 << 29, "1000": 1000} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1MB", "lots"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}

func TestMaskArgs(t *testing.T) {
	got := maskArgs([]string{"report", "--note", "my token abc", "--redact-extra=secret\\d+", "-package", "com.x", "-note=x", "--keep-domain", "corp.example"})
	want := []string{"report", "--note", "<omitted>", "--redact-extra=<omitted>", "-package", "com.x", "-note=<omitted>", "--keep-domain", "<omitted>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("maskArgs = %q", got)
	}
}
