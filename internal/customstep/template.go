package customstep

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/manent-deployer/manent/internal/customspec"
	"github.com/manent-deployer/manent/internal/pipeline"
)

// templateData is the placeholder data. Values like Version and Params come from whoever
// releases or from the environment configuration: they are not trusted and are checked or
// encoded according to the context they end up in.
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
	// Response is the first JSON response, for poll.url only.
	Response any
}

// render runs a template; a missing key is an error.
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

// renderArg produces a command argument. A substituted value cannot make it empty, add
// control characters or turn it into an option (argument injection).
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
		return "", fmt.Errorf("%s: the substituted value is empty", name)
	case strings.ContainsAny(out, "\x00\r\n"):
		return "", fmt.Errorf("%s: the substituted value contains control characters", name)
	case strings.HasPrefix(out, "-") && !strings.HasPrefix(text, "-"):
		return "", fmt.Errorf("%s: the substituted value starts with '-' and would be read as an option", name)
	}
	return out, nil
}

// renderURL produces a URL: every substituted value is encoded (only unreserved characters
// stay in clear), so it cannot add segments, query or fragments, and scheme and host stay
// those written in the spec.
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
		return nil, fmt.Errorf("%s: the destination does not match the one of the specification", name)
	}
	// A "." or ".." value would climb the path (path traversal).
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if v, err := url.PathUnescape(seg); err != nil || v == "." || v == ".." {
			return nil, fmt.Errorf("%s: path segment not allowed", name)
		}
	}
	return u, nil
}

// renderHeader produces a header value, without newlines.
func renderHeader(name, text string, data templateData) (string, error) {
	out, err := render(name, text, data)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(out, "\x00\r\n") {
		return "", fmt.Errorf("%s: the value contains control characters", name)
	}
	return out, nil
}

// renderJSON replaces the placeholders only in the string values of the structure: the body
// is then serialized by manent, so a value cannot alter its structure.
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

// escaped returns a copy of the data with strings encoded for a URL.
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

// escape percent-encodes everything except the unreserved characters of RFC 3986: the
// result is safe both in a path segment and in the query.
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
