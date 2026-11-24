package server

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

func etagMatches(header, tag string) bool {
	tag = strings.TrimPrefix(tag, "W/")
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == tag {
			return true
		}
	}
	return false
}
