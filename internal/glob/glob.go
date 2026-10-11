// Package glob confronta stringhe con pattern semplici, usati nelle policy di configurazione.
package glob

import "strings"

// Match indica se s corrisponde al pattern, in cui "*" vale qualsiasi sequenza di caratteri
// (anche vuota e anche con "/"). Gli altri caratteri sono confrontati alla lettera.
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

// Any indica se s corrisponde ad almeno uno dei pattern.
func Any(patterns []string, s string) bool {
	for _, p := range patterns {
		if Match(p, s) {
			return true
		}
	}
	return false
}
