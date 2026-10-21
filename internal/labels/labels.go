// Package labels parses the selectors on target labels, for example
// "region=eu-1, az in (eu-1a, eu-1b), canary!=true". Comma-separated conditions must all
// hold. It depends on no other package: descriptor and rollout use it.
package labels

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Reserved target labels.
const (
	Region = "region"
	AZ     = "az"
)

// Operators of a condition.
const (
	opEq    = "="
	opNe    = "!="
	opIn    = "in"
	opNotIn = "notin"
)

// requirement is a condition on a label.
type requirement struct {
	key    string
	op     string
	values []string
}

// Selector is a set of conditions, all required.
type Selector struct {
	reqs []requirement
	text string
}

// String returns the selector as written.
func (s Selector) String() string { return s.text }

// Keys returns the label keys the selector reads, in order of appearance.
func (s Selector) Keys() []string {
	var out []string
	for _, r := range s.reqs {
		if !slices.Contains(out, r.key) {
			out = append(out, r.key)
		}
	}
	return out
}

var (
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	valueRe = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
)

// Parse parses a selector. An empty selector is not allowed: it would select everything.
func Parse(text string) (Selector, error) {
	s := Selector{text: strings.TrimSpace(text)}
	if s.text == "" {
		return s, errors.New("empty selector")
	}
	for _, part := range splitTop(s.text) {
		r, err := parseRequirement(strings.TrimSpace(part))
		if err != nil {
			return s, fmt.Errorf("selector %q: %w", s.text, err)
		}
		s.reqs = append(s.reqs, r)
	}
	return s, nil
}

// splitTop splits on commas outside parentheses.
func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func parseRequirement(p string) (requirement, error) {
	if p == "" {
		return requirement{}, errors.New("empty condition")
	}
	if k, rest, ok := cutWord(p, " notin "); ok {
		return setRequirement(k, opNotIn, rest)
	}
	if k, rest, ok := cutWord(p, " in "); ok {
		return setRequirement(k, opIn, rest)
	}
	for _, op := range []string{opNe, opEq} {
		if k, v, ok := strings.Cut(p, op); ok {
			r := requirement{key: strings.TrimSpace(k), op: op, values: []string{strings.TrimSpace(v)}}
			return r, r.check()
		}
	}
	return requirement{}, fmt.Errorf("invalid condition %q (key=value, key!=value, key in (a, b), key notin (a, b))", p)
}

func cutWord(p, word string) (string, string, bool) {
	i := strings.Index(strings.ToLower(p), word)
	if i < 0 {
		return "", "", false
	}
	return p[:i], p[i+len(word):], true
}

func setRequirement(key, op, rest string) (requirement, error) {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return requirement{}, fmt.Errorf("%s requires a list in parentheses", op)
	}
	r := requirement{key: strings.TrimSpace(key), op: op}
	for _, v := range strings.Split(rest[1:len(rest)-1], ",") {
		r.values = append(r.values, strings.TrimSpace(v))
	}
	return r, r.check()
}

func (r requirement) check() error {
	if !keyRe.MatchString(r.key) {
		return fmt.Errorf("invalid key %q", r.key)
	}
	for _, v := range r.values {
		if !valueRe.MatchString(v) {
			return fmt.Errorf("invalid value %q", v)
		}
	}
	if (r.op == opIn || r.op == opNotIn) && (len(r.values) == 0 || slices.Contains(r.values, "")) {
		return fmt.Errorf("%s: empty list or with empty values", r.op)
	}
	return nil
}

// Matches reports whether the labels satisfy all conditions. A missing label counts as "".
func (s Selector) Matches(l map[string]string) bool {
	for _, r := range s.reqs {
		v := l[r.key]
		switch r.op {
		case opEq:
			if v != r.values[0] {
				return false
			}
		case opNe:
			if v == r.values[0] {
				return false
			}
		case opIn:
			if !slices.Contains(r.values, v) {
				return false
			}
		case opNotIn:
			if slices.Contains(r.values, v) {
				return false
			}
		}
	}
	return true
}

// ValidLabels checks keys and values of a target's labels.
func ValidLabels(l map[string]string) error {
	for k, v := range l {
		if !keyRe.MatchString(k) || !valueRe.MatchString(v) {
			return fmt.Errorf("invalid label %s=%s", k, v)
		}
	}
	return nil
}
