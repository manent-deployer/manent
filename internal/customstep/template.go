package customstep

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// templateData sono i dati dei segnaposto. Valori come Version e Params arrivano da chi
// rilascia o dalla configurazione di ambiente: non sono fidati e vengono controllati o
// codificati in base al contesto in cui finiscono.
type templateData struct {
	Application  string
	Environment  string
	Version      string
	Artifact     string
	DeploymentID string
	SHA256       string
	DryRun       bool
	Params       map[string]string
	Outputs      map[string]pipeline.Output
	// Response è la prima risposta JSON, solo per poll.url.
	Response any
}

// render esegue un template; una chiave mancante è un errore.
func render(name, text string, data any) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	t, err := customspec.NewTemplate(name, text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return b.String(), nil
}

// renderArg produce un argomento del comando. Un valore sostituito non può renderlo vuoto,
// introdurre caratteri di controllo o trasformarlo in un'opzione (argument injection).
func renderArg(i int, text string, data templateData) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	name := fmt.Sprintf("command[%d]", i)
	out, err := render(name, text, data)
	switch {
	case err != nil:
		return "", err
	case out == "":
		return "", fmt.Errorf("%s: il valore sostituito è vuoto", name)
	case strings.ContainsAny(out, "\x00\r\n"):
		return "", fmt.Errorf("%s: il valore sostituito contiene caratteri di controllo", name)
	case strings.HasPrefix(out, "-") && !strings.HasPrefix(text, "-"):
		return "", fmt.Errorf("%s: il valore sostituito inizia con '-' e sarebbe letto come un'opzione", name)
	}
	return out, nil
}

// renderURL produce un URL: ogni valore sostituito è codificato (solo i caratteri non
// riservati restano in chiaro), quindi non può aggiungere segmenti, query o frammenti, e
// schema e host restano quelli scritti nella specifica.
func renderURL(name, text string, data templateData, allowHTTP bool) (*url.URL, error) {
	origin, err := customspec.LiteralOrigin(text, allowHTTP)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out, err := render(name, text, data.escaped())
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if u.Scheme+"://"+u.Host != origin || u.User != nil {
		return nil, fmt.Errorf("%s: la destinazione non corrisponde a quella della specifica", name)
	}
	// Un valore "." o ".." risalirebbe il percorso (path traversal).
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if v, err := url.PathUnescape(seg); err != nil || v == "." || v == ".." {
			return nil, fmt.Errorf("%s: segmento del percorso non ammesso", name)
		}
	}
	return u, nil
}

// renderHeader produce il valore di un header, senza a capo.
func renderHeader(name, text string, data templateData) (string, error) {
	out, err := render(name, text, data)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(out, "\x00\r\n") {
		return "", fmt.Errorf("%s: il valore contiene caratteri di controllo", name)
	}
	return out, nil
}

// renderJSON sostituisce i segnaposto nei soli valori stringa della struttura: il corpo è
// poi serializzato da manent, quindi un valore non può alterarne la struttura.
func renderJSON(v any, path string, data templateData) (any, error) {
	switch t := v.(type) {
	case string:
		return render(path, t, data)
	case map[string]any:
		out := make(map[string]any, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			r, err := renderJSON(t[k], path+"."+k, data)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := renderJSON(e, fmt.Sprintf("%s[%d]", path, i), data)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// escaped restituisce una copia dei dati con le stringhe codificate per un URL.
func (d templateData) escaped() templateData {
	e := d
	e.Application, e.Environment, e.Version = escape(d.Application), escape(d.Environment), escape(d.Version)
	e.Artifact, e.DeploymentID, e.SHA256 = escape(d.Artifact), escape(d.DeploymentID), escape(d.SHA256)
	e.Params = map[string]string{}
	for k, v := range d.Params {
		e.Params[k] = escape(v)
	}
	e.Outputs = map[string]pipeline.Output{}
	for step, out := range d.Outputs {
		o := pipeline.Output{}
		for k, v := range out {
			o[k] = escape(v)
		}
		e.Outputs[step] = o
	}
	e.Response = escapeAny(d.Response)
	return e
}

func escapeAny(v any) any {
	switch t := v.(type) {
	case string:
		return escape(t)
	case json.Number:
		return escape(t.String())
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = escapeAny(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = escapeAny(e)
		}
		return out
	}
	return v
}

// escape codifica in percentuale tutto tranne i caratteri non riservati di RFC 3986: il
// risultato è sicuro sia in un segmento del percorso sia nella query.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
