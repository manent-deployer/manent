// Package glob matches strings against simple patterns, used in configuration policies.
package glob

import "strings"

// Match reports whether s matches the pattern, where "*" stands for any sequence of
// characters (also empty and also with "/"). Other characters are compared literally.
func Match(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, last)
}

// Any reports whether s matches at least one of the patterns.
func Any(patterns []string, s string) bool {
	for _, p := range patterns {
		if Match(p, s) {
			return true
		}
	}
	return false
}
