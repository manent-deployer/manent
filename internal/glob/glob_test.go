package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "qualsiasi/cosa", true},
		{"myapp", "myapp", true},
		{"myapp", "myapp2", false},
		{"team-a-*", "team-a-api", true},
		{"team-a-*", "team-b-api", false},
		{"vault:secret/data/myapp/*", "vault:secret/data/myapp/prod#key", true},
		{"vault:secret/data/myapp/*", "vault:secret/data/other/prod#key", false},
		{"*-prod", "eu-prod", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXcYb", false},
		{"ab*ba", "aba", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.s); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, atteso %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}
