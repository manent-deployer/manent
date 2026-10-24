package customstep

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// redactor replaces secret values with ***, also in the most common encoded forms.
// It is an extra defense: it does not recognize a split, truncated or transformed secret.
type redactor struct {
	values []string
	repl   *strings.Replacer
}

// minRedact is the minimum length of a value to redact: shorter values would cause
// replacements in arbitrary text.
const minRedact = 4

func newRedactor(secrets []string) *redactor {
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
	return &redactor{values: values, repl: strings.NewReplacer(pairs...)}
}

func (r *redactor) apply(s string) string {
	if len(r.values) == 0 {
		return s
	}
	return r.repl.Replace(s)
}

func (r *redactor) contains(s string) bool {
	for _, v := range r.values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// err redacts the error message keeping its nature (permanent or temporary).
func (r *redactor) err(e error) error {
	msg := r.apply(e.Error())
	if msg == e.Error() {
		return e
	}
	out := errors.New(msg)
	if errors.Is(e, pipeline.ErrPermanent) {
		return pipeline.Permanent(out)
	}
	return out
}

// Limits of command output in the deploy log.
const (
	maxLogBytes = 1 << 20 // per run, across all targets
	maxLogLine  = 4 << 10
)

// logBudget is the byte limit shared by the streams of a run, written in parallel by stdout
// and stderr.
type logBudget struct {
	mu        sync.Mutex
	left      int
	truncated bool
}

// lineWriter writes a command's output to the deploy log, line by line, redacted and limited:
// a process that writes endlessly fills neither the worker's memory nor its disk.
type lineWriter struct {
	ctx    context.Context
	log    *slog.Logger
	red    *redactor
	budget *logBudget
	attrs  []any
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if w.budget.left <= 0 {
			if !w.budget.truncated {
				w.budget.truncated = true
				w.log.WarnContext(w.ctx, "command output truncated in the log", w.attrs...)
			}
			w.buf = w.buf[:0]
			return n, nil
		}
		i := slices.Index(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		take := min(len(chunk), w.budget.left, maxLogLine-len(w.buf))
		w.buf = append(w.buf, chunk[:take]...)
		w.budget.left -= take
		if i >= 0 && take == len(chunk) {
			w.flush()
			p = p[i+1:]
			continue
		}
		p = p[take:]
		if len(w.buf) >= maxLogLine {
			// A line that is too long continues on the next one.
			w.flush()
		}
	}
	return n, nil
}

// flush writes the accumulated line.
func (w *lineWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	line := strings.TrimRight(string(w.buf), "\r")
	w.buf = w.buf[:0]
	w.log.InfoContext(w.ctx, w.red.apply(line), w.attrs...)
}
