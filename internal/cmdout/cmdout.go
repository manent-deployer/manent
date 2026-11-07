// Package cmdout handles the output of the commands manent runs for a deploy (custom steps,
// the script plugin): secret redaction, the deploy log line by line within a budget and the
// key=value outputs file.
package cmdout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Redactor replaces secret values with ***, also in the most common encoded forms.
// It is an extra defense: it does not recognize a split, truncated or transformed secret.
type Redactor struct {
	values []string
	repl   *strings.Replacer
}

// minRedact is the minimum length of a value to redact: shorter values would cause
// replacements in arbitrary text.
const minRedact = 4

// NewRedactor builds the redactor of the given secret values.
func NewRedactor(secrets []string) *Redactor {
	set := map[string]bool{}
	add := func(v string) {
		if len(v) >= minRedact {
			set[v] = true
		}
	}
	for _, s := range secrets {
		for _, v := range []string{s, strings.TrimSpace(s)} {
			add(v)
			add(base64.StdEncoding.EncodeToString([]byte(v)))
			add(base64.RawURLEncoding.EncodeToString([]byte(v)))
			add(url.QueryEscape(v))
			add(url.PathEscape(v))
			if b, err := json.Marshal(v); err == nil {
				add(string(b[1 : len(b)-1]))
			}
		}
		// Private keys and other multi-line secrets often appear line by line.
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); len(line) >= 8 && !strings.HasPrefix(line, "-----") {
				set[line] = true
			}
		}
	}
	values := make([]string, 0, len(set))
	for v := range set {
		values = append(values, v)
	}
	// Longer values first: a value must not leave part of a longer one in clear.
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, "***")
	}
	return &Redactor{values: values, repl: strings.NewReplacer(pairs...)}
}

// Apply redacts s.
func (r *Redactor) Apply(s string) string {
	if len(r.values) == 0 {
		return s
	}
	return r.repl.Replace(s)
}

// Contains reports whether s contains a secret value.
func (r *Redactor) Contains(s string) bool {
	for _, v := range r.values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// MaxLine is the longest line written to the log: a longer one continues on the next.
const MaxLine = 4 << 10

// Budget is the byte limit shared by the streams of a run, written in parallel by stdout
// and stderr.
type Budget struct {
	mu        sync.Mutex
	left      int
	truncated bool
}

// NewBudget returns a budget of n bytes.
func NewBudget(n int) *Budget { return &Budget{left: n} }

// LineWriter writes a command's output to the deploy log, line by line, redacted and limited:
// a process that writes endlessly fills neither the worker's memory nor its disk. Call Flush
// when the command ends, for the last line without a newline.
type LineWriter struct {
	Ctx    context.Context
	Log    *slog.Logger
	Red    *Redactor
	Budget *Budget
	Attrs  []any
	buf    []byte
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.Budget.mu.Lock()
	defer w.Budget.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if w.Budget.left <= 0 {
			if !w.Budget.truncated {
				w.Budget.truncated = true
				w.Log.WarnContext(w.Ctx, "command output truncated in the log", w.Attrs...)
			}
			w.buf = w.buf[:0]
			return n, nil
		}
		i := slices.Index(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		take := min(len(chunk), w.Budget.left, MaxLine-len(w.buf))
		w.buf = append(w.buf, chunk[:take]...)
		w.Budget.left -= take
		if i >= 0 && take == len(chunk) {
			w.Flush()
			p = p[i+1:]
			continue
		}
		p = p[take:]
		if len(w.buf) >= MaxLine {
			// A line that is too long continues on the next one.
			w.Flush()
		}
	}
	return n, nil
}

// Flush writes the accumulated line.
func (w *LineWriter) Flush() {
	if len(w.buf) == 0 {
		return
	}
	line := strings.TrimRight(string(w.buf), "\r")
	w.buf = w.buf[:0]
	w.Log.InfoContext(w.Ctx, w.Red.Apply(line), w.Attrs...)
}

// ParseOutputs reads the key=value lines of an outputs file; empty lines and lines starting
// with # are skipped.
func ParseOutputs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, errors.New("invalid output: key=value lines")
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("output %q repeated", k)
		}
		out[k] = v
	}
	return out, nil
}
