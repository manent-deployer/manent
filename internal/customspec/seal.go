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

// Document è la forma fotografata nel piano: nome dello step e specifica. Il nome fa parte
// della firma, così una specifica non può essere spostata su un altro step.
type Document struct {
	Name string `json:"name"`
	Spec
}

// Encode restituisce la forma canonica (JSON con chiavi ordinate) della specifica.
func Encode(name string, s Spec) (string, error) {
	b, err := json.Marshal(Document{Name: name, Spec: s})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Decode legge una specifica fotografata nel piano.
func Decode(doc string) (Document, error) {
	var d Document
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber() // i numeri del corpo JSON restano esatti
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("specifica dello step non leggibile: %w", err)
	}
	return d, nil
}

// Hash è lo sha256 esadecimale della forma canonica: identifica la specifica, anche nelle revoche.
func Hash(doc string) string {
	h := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(h[:])
}

// macDomain separa le firme delle specifiche da altri usi della stessa chiave.
const macDomain = "manent-step-spec-v1\n"

// Keys sono le chiavi di firma: la prima firma, tutte verificano (rotazione).
type Keys [][]byte

// MinKeyLen è la lunghezza minima di una chiave.
const MinKeyLen = 32

// Sign firma la forma canonica con la prima chiave; senza chiavi restituisce "".
func (k Keys) Sign(doc string) string {
	if len(k) == 0 {
		return ""
	}
	return mac(k[0], doc)
}

// Verify controlla la firma. Senza chiavi configurate accetta solo specifiche non firmate;
// con le chiavi, una specifica senza firma o con una firma non valida è rifiutata.
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

// EffectiveParams calcola i parametri dello step: default della specifica, poi i valori
// configurati. complete indica che la configurazione di ambiente è stata letta: solo allora
// un parametro obbligatorio mancante è un errore.
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

// DeclaredSecrets sono tutti i segreti che lo step può usare: run, dry_run e SSH.
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

// PathStep è un passo di un percorso JSON: una chiave o un indice.
type PathStep struct {
	Key   string
	Index int
	IsKey bool
}

// ParsePath legge un percorso nella forma $.a.b[0]["c"]: un sottoinsieme di JSONPath senza
// filtri né caratteri jolly, così il valore estratto è sempre uno solo.
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

// Lookup estrae dal documento JSON decodificato il valore scalare indicato da path.
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
