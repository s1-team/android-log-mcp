package service

import (
	"strconv"
	"strings"
)

// Proto is the client/service protocol version. Bump it on any
// incompatible change of the ops or their bodies; raise MinProto when the
// service can no longer serve clients of an older protocol.
var (
	Proto    = 1
	MinProto = 1
)

// CompareVersions compares two release versions (semver: MAJOR.MINOR.PATCH,
// a pre-release sorts before its release, build metadata is ignored).
// Unparseable versions compare equal (no replacement either way).
func CompareVersions(a, b string) int {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka || !okb {
		return 0
	}
	for i := 0; i < 3; i++ {
		if va.num[i] != vb.num[i] {
			if va.num[i] < vb.num[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case va.pre == "" && vb.pre == "":
		return 0
	case va.pre == "":
		return 1
	case vb.pre == "":
		return -1
	}
	pa, pb := strings.Split(va.pre, "."), strings.Split(vb.pre, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if c := cmpIdent(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}

type version struct {
	num [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	var v version
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	s, v.pre, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.num[i] = n
	}
	return v, true
}

func cmpIdent(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case ea == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// ProtoCompatible reports whether a client speaking proto can use a
// service speaking [minProto, serviceProto].
func ProtoCompatible(proto, serviceMin, serviceProto int) bool {
	return proto >= serviceMin && proto <= serviceProto
}
