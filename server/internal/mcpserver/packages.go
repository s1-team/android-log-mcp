package mcpserver

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxPackages caps the structured package parameter (#276); same as tags.
const maxPackages = 20

// PackageList is one or more exact application ids for the structured
// `package` parameter (#276). JSON may be a string or an array of strings
// (same shape as tags). A string may be a single id or comma-separated ids
// (comma is the only delimiter; application ids do not contain commas).
// Cap is maxPackages; callers reject over-cap with bad_param.
type PackageList []string

// UnmarshalJSON accepts a JSON string, a JSON array of strings, or null.
func (p *PackageList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*p = nil
		return nil
	}
	if b[0] == '[' {
		var ss []string
		if err := json.Unmarshal(b, &ss); err != nil {
			return err
		}
		*p = normalizePackageList(ss)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*p = splitPackageString(s)
	return nil
}

// MarshalJSON encodes a single id as a string (compat) and multiple as an array.
func (p PackageList) MarshalJSON() ([]byte, error) {
	switch len(p) {
	case 0:
		return []byte("null"), nil
	case 1:
		return json.Marshal(p[0])
	default:
		return json.Marshal([]string(p))
	}
}

func splitPackageString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if !strings.Contains(s, ",") {
		return []string{s}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return normalizePackageList(out)
}

// normalizePackageList trims, drops empties, and dedupes preserving first-seen order.
func normalizePackageList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// packageHeader formats the package= header value: a single id bare, or
// comma-separated ids in request order (parseable; app ids have no commas).
func packageHeader(pkgs []string) string {
	return strings.Join(pkgs, ",")
}

func packageSet(pkgs []string) map[string]bool {
	if len(pkgs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		m[p] = true
	}
	return m
}

// checkPackages rejects more than maxPackages ids (bad_param).
func checkPackages(pkgs []string) error {
	if len(pkgs) > maxPackages {
		return errc("bad_param", []string{`package=["first 20 ids"]`}, "param", "package", "value", len(pkgs), "valid", "max 20")
	}
	return nil
}
