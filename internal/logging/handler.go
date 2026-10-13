package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/crewjam/rfc5424"
)

// sdID è l'identificativo dello structured data RFC 5424 usato per gli attributi slog.
const sdID = "manent@32473"

// facilityLocal0 è la facility local0 (16) di RFC 5424. rfc5424.Local0 vale 12, perché la
// libreria omette le facility da 12 a 15 (ntp, security, console, solaris-cron).
const facilityLocal0 rfc5424.Priority = 16 << 3

// Options configura la costruzione del logger.
type Options struct {
	AppName string
	Level   string
	Format  string
	Output  io.Writer
	SIEM    SIEMSender
}

// New crea un *slog.Logger che scrive sul log applicativo e inoltra ogni record al SIEM.
func New(opts Options) *slog.Logger {
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	if opts.SIEM == nil {
		opts.SIEM = NoopSender{}
	}
	hopts := &slog.HandlerOptions{Level: parseLevel(opts.Level)}
	var app slog.Handler
	if opts.Format == "json" {
		app = slog.NewJSONHandler(opts.Output, hopts)
	} else {
		app = slog.NewTextHandler(opts.Output, hopts)
	}
	host, _ := os.Hostname()
	siem := &siemHandler{
		sender:  opts.SIEM,
		level:   hopts.Level,
		appName: opts.AppName,
		host:    host,
		pid:     strconv.Itoa(os.Getpid()),
		errOut:  opts.Output,
	}
	return slog.New(&fanoutHandler{handlers: []slog.Handler{app, siem}})
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// fanoutHandler inoltra ogni record a tutti gli handler abilitati.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: hs}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: hs}
}

// siemHandler converte i record slog in rfc5424.Message e li invia al SIEM.
type siemHandler struct {
	sender  SIEMSender
	level   slog.Leveler
	appName string
	host    string
	pid     string
	prefix  string
	attrs   []rfc5424.SDParam
	errOut  io.Writer
}

func (h *siemHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *siemHandler) Handle(_ context.Context, r slog.Record) error {
	msg := rfc5424.Message{
		Priority:  facilityLocal0 | severity(r.Level),
		Timestamp: r.Time,
		Hostname:  h.host,
		AppName:   h.appName,
		ProcessID: h.pid,
		MessageID: "-",
		Message:   []byte(r.Message),
	}
	params := append([]rfc5424.SDParam(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		params = appendAttr(params, h.prefix, a)
		return true
	})
	if len(params) > 0 {
		msg.StructuredData = []rfc5424.StructuredData{{ID: sdID, Parameters: params}}
	}
	// Un SIEM irraggiungibile non deve bloccare l'applicazione: l'errore viene solo segnalato.
	if err := h.sender.Send(msg); err != nil {
		fmt.Fprintf(h.errOut, "invio al SIEM fallito: %v\n", err)
	}
	return nil
}

func (h *siemHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]rfc5424.SDParam(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = appendAttr(c.attrs, h.prefix, a)
	}
	return &c
}

func (h *siemHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

// appendAttr appiattisce gli attributi (anche di gruppo) in parametri SD.
func appendAttr(params []rfc5424.SDParam, prefix string, a slog.Attr) []rfc5424.SDParam {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			params = appendAttr(params, p, ga)
		}
		return params
	}
	if a.Key == "" {
		return params
	}
	return append(params, rfc5424.SDParam{Name: sdName(prefix + a.Key), Value: a.Value.String()})
}

// sdName rende il nome conforme a RFC 5424 (ASCII stampabile, niente '=', ' ', ']', '"', max 32).
func sdName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r <= 32 || r >= 127 || r == '=' || r == ']' || r == '"' {
			return '_'
		}
		return r
	}, s)
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

func severity(l slog.Level) rfc5424.Priority {
	switch {
	case l >= slog.LevelError:
		return rfc5424.Error
	case l >= slog.LevelWarn:
		return rfc5424.Warning
	case l >= slog.LevelInfo:
		return rfc5424.Info
	default:
		return rfc5424.Debug
	}
}
