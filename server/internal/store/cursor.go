package store

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Cursor is an opaque position: "c1.<b64url serial>.<boot>.<seq>".
type Cursor struct {
	Serial string
	Boot   string
	Seq    uint64
}

func (c Cursor) String() string {
	return fmt.Sprintf("c1.%s.%s.%d", base64.RawURLEncoding.EncodeToString([]byte(c.Serial)), c.Boot, c.Seq)
}

// ParseCursor validates and decodes a cursor string.
func ParseCursor(s string) (Cursor, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 || parts[0] != "c1" {
		return Cursor{}, fmt.Errorf("malformed cursor %q: expected a value like c1.ZW11.9f2a1b3c.1234 copied from a previous response", s)
	}
	ser, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Cursor{}, fmt.Errorf("malformed cursor %q (bad device part)", s)
	}
	seq, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("malformed cursor %q (bad sequence)", s)
	}
	return Cursor{Serial: string(ser), Boot: parts[2], Seq: seq}, nil
}
