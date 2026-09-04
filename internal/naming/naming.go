// Package naming builds stable, human-friendly device ids from vendor/model
// strings. Shared by the discovery poller and the adopt/provision pipeline.
package naming

import (
	"fmt"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug joins parts, lowercases, and collapses non-alphanumerics to single
// dashes, e.g. Slug("Xiaomi", "Redmi 8A") -> "xiaomi-redmi-8a".
func Slug(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "-"))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "device"
	}
	return s
}

// ShortSerial returns the first 8 chars of a serial/udid, for provisional ids.
func ShortSerial(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// UniqueID returns base, or base-2, base-3, ... until it is not in taken.
func UniqueID(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[cand] {
			return cand
		}
	}
}
