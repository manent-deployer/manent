package customspec

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Document is the form captured in the plan: step name and spec. The name is part of the
// signature, so a spec cannot be moved to another step.
type Document struct {
	Name string `json:"name"`
	Spec
}

// Encode returns the canonical form (JSON with sorted keys) of the spec.
func Encode(name string, s Spec) (string, error) {
	b, err := json.Marshal(Document{Name: name, Spec: s})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Decode reads a spec captured in the plan.
func Decode(doc string) (Document, error) {
	var d Document
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber() // numbers of the JSON body stay exact
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("specifica dello step non leggibile: %w", err)
	}
	return d, nil
}

// Hash is the hex sha256 of the canonical form: it identifies the spec, also in revocations.
func Hash(doc string) string {
	h := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(h[:])
}

// macDomain separates spec signatures from other uses of the same key.
const macDomain = "manent-step-spec-v1\n"

// Keys are the signing keys: the first signs, all verify (rotation).
type Keys [][]byte

// MinKeyLen is the minimum length of a key.
const MinKeyLen = 32

// Sign signs the canonical form with the first key; without keys it returns "".
func (k Keys) Sign(doc string) string {
	if len(k) == 0 {
		return ""
	}
	return mac(k[0], doc)
}

// Verify checks the signature. Without configured keys it accepts only unsigned specs; with
// keys, a spec without a signature or with an invalid one is rejected.
func (k Keys) Verify(doc, sig string) error {
	if len(k) == 0 {
		return nil
	}
	if sig == "" {
		return errors.New("specifica dello step non firmata: con pipeline.custom.spec_key_refs ogni specifica deve essere firmata")
	}
	for _, key := range k {
		if hmac.Equal([]byte(mac(key, doc)), []byte(sig)) {
			return nil
		}
	}
	return errors.New("firma della specifica dello step non valida: il piano è stato alterato o la chiave non è più tra quelle configurate")
}

func mac(key []byte, doc string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(macDomain))
	m.Write([]byte(doc))
	return hex.EncodeToString(m.Sum(nil))
}

// EffectiveParams computes the step parameters: spec defaults, then configured values.
// complete means the environment configuration was read: only then is a missing required
// parameter an error.
func (s Spec) EffectiveParams(values map[string]string, complete bool) (map[string]string, error) {
	out := map[string]string{}
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if _, ok := s.Params[k]; !ok {
			errs = append(errs, fmt.Errorf("parametro %q non dichiarato dallo step", k))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Params)) {
		p := s.Params[k]
		v := values[k]
		if v == "" {
			v = p.Default
		}
		if v == "" {
			if p.Required && complete {
				errs = append(errs, fmt.Errorf("parametro %q obbligatorio", k))
			}
			continue
		}
		re, err := Compile(p.Pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("parametro %q: %w", k, err))
			continue
		}
		if !re.MatchString(v) {
			errs = append(errs, fmt.Errorf("parametro %q: valore non conforme al pattern dello step", k))
			continue
		}
		out[k] = v
	}
	return out, errors.Join(errs...)
}

// DeclaredSecrets are all the secrets the step can use: run, dry_run and SSH.
func (s Spec) DeclaredSecrets() []string {
	names := slices.Clone(s.Secrets)
	if s.DryRun != nil {
		names = append(names, s.DryRun.Secrets...)
	}
	if s.SSH != nil {
		names = append(names, nonEmpty(s.SSH.KeySecret, s.SSH.PasswordSecret)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// PathStep is a step of a JSON path: a key or an index.
type PathStep struct {
	Key   string
	Index int
	IsKey bool
}

// ParsePath reads a path of the form $.a.b[0]["c"]: a subset of JSONPath without filters or
// wildcards, so the extracted value is always a single one.
func ParsePath(p string) ([]PathStep, error) {
	if !strings.HasPrefix(p, "$") {
		return nil, errors.New("il percorso inizia con $")
	}
	var out []PathStep
	rest := p[1:]
	for rest != "" {
		switch {
		case rest[0] == '.':
			end := strings.IndexAny(rest[1:], ".[")
			if end < 0 {
				end = len(rest) - 1
			}
			key := rest[1 : end+1]
			if key == "" || key == "*" {
				return nil, fmt.Errorf("chiave vuota o non ammessa in %q", p)
			}
			out = append(out, PathStep{Key: key, IsKey: true})
			rest = rest[end+1:]
		case strings.HasPrefix(rest, `["`):
			end := strings.Index(rest, `"]`)
			if end < 2 {
				return nil, fmt.Errorf("chiave tra parentesi non chiusa in %q", p)
			}
			out = append(out, PathStep{Key: rest[2:end], IsKey: true})
			rest = rest[end+2:]
		case rest[0] == '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, fmt.Errorf("indice non chiuso in %q", p)
			}
			i, err := strconv.Atoi(rest[1:end])
			if err != nil || i < 0 {
				return nil, fmt.Errorf("indice non valido in %q", p)
			}
			out = append(out, PathStep{Index: i})
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("percorso non valido: %q", p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("il percorso deve indicare almeno un campo")
	}
	return out, nil
}

// Lookup extracts from the decoded JSON document the scalar value at path.
func Lookup(doc any, path string) (string, bool) {
	steps, err := ParsePath(path)
	if err != nil {
		return "", false
	}
	cur := doc
	for _, s := range steps {
		switch v := cur.(type) {
		case map[string]any:
			if !s.IsKey {
				return "", false
			}
			cur = v[s.Key]
		case []any:
			if s.IsKey || s.Index >= len(v) {
				return "", false
			}
			cur = v[s.Index]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	}
	return "", false
}
