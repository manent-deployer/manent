// Package labels interpreta i selettori sulle etichette dei target, ad esempio
// "region=eu-1, az in (eu-1a, eu-1b), canary!=true". Le condizioni separate da virgole
// devono valere tutte. Non dipende da altri package: lo usano descrittore e rollout.
package labels

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Etichette riservate dei target.
const (
	Region = "region"
	AZ     = "az"
)

// Operatori di una condizione.
const (
	opEq    = "="
	opNe    = "!="
	opIn    = "in"
	opNotIn = "notin"
)

// requirement è una condizione su un'etichetta.
type requirement struct {
	key    string
	op     string
	values []string
}

// Selector è un insieme di condizioni, tutte necessarie.
type Selector struct {
	reqs []requirement
	text string
}

// String restituisce il selettore come è stato scritto.
func (s Selector) String() string { return s.text }

var (
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	valueRe = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
)

// Parse interpreta un selettore. Un selettore vuoto non è ammesso: selezionerebbe tutto.
func Parse(text string) (Selector, error) {
	s := Selector{text: strings.TrimSpace(text)}
	if s.text == "" {
		return s, errors.New("selettore vuoto")
	}
	for _, part := range splitTop(s.text) {
		r, err := parseRequirement(strings.TrimSpace(part))
		if err != nil {
			return s, fmt.Errorf("selettore %q: %w", s.text, err)
		}
		s.reqs = append(s.reqs, r)
	}
	return s, nil
}

// splitTop divide sulle virgole fuori dalle parentesi.
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
		return requirement{}, errors.New("condizione vuota")
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
	return requirement{}, fmt.Errorf("condizione %q non valida (chiave=valore, chiave!=valore, chiave in (a, b), chiave notin (a, b))", p)
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
		return requirement{}, fmt.Errorf("%s richiede un elenco tra parentesi", op)
	}
	r := requirement{key: strings.TrimSpace(key), op: op}
	for _, v := range strings.Split(rest[1:len(rest)-1], ",") {
		r.values = append(r.values, strings.TrimSpace(v))
	}
	return r, r.check()
}

func (r requirement) check() error {
	if !keyRe.MatchString(r.key) {
		return fmt.Errorf("chiave %q non valida", r.key)
	}
	for _, v := range r.values {
		if !valueRe.MatchString(v) {
			return fmt.Errorf("valore %q non valido", v)
		}
	}
	if (r.op == opIn || r.op == opNotIn) && (len(r.values) == 0 || slices.Contains(r.values, "")) {
		return fmt.Errorf("%s: elenco vuoto o con valori vuoti", r.op)
	}
	return nil
}

// Matches indica se le etichette soddisfano tutte le condizioni. Un'etichetta assente vale "".
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

// ValidLabels verifica chiavi e valori delle etichette di un target.
func ValidLabels(l map[string]string) error {
	for k, v := range l {
		if !keyRe.MatchString(k) || !valueRe.MatchString(v) {
			return fmt.Errorf("etichetta %s=%s non valida", k, v)
		}
	}
	return nil
}
