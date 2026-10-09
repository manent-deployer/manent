package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crewjam/rfc5424"
)

// fakeSender memorizza i messaggi ricevuti per le verifiche.
type fakeSender struct{ msgs []rfc5424.Message }

func (f *fakeSender) Send(m rfc5424.Message) error { f.msgs = append(f.msgs, m); return nil }
func (f *fakeSender) Close() error                 { return nil }

func TestLoggerScriveSuAppESIEM(t *testing.T) {
	var out bytes.Buffer
	siem := &fakeSender{}
	log := New(Options{AppName: "manent", Level: "info", Output: &out, SIEM: siem})

	log.With("deployment", "d1").WithGroup("step").Warn("step fallito", "name", "qa")
	log.Debug("ignorato")

	if !strings.Contains(out.String(), "step fallito") {
		t.Fatalf("log applicativo mancante: %q", out.String())
	}
	if len(siem.msgs) != 1 {
		t.Fatalf("attesi 1 messaggio SIEM, ricevuti %d", len(siem.msgs))
	}
	m := siem.msgs[0]
	if m.Priority != rfc5424.Local0|rfc5424.Warning {
		t.Errorf("priorità errata: %v", m.Priority)
	}
	if _, err := m.MarshalBinary(); err != nil {
		t.Fatalf("messaggio RFC 5424 non valido: %v", err)
	}
	got := map[string]string{}
	for _, p := range m.StructuredData[0].Parameters {
		got[p.Name] = p.Value
	}
	if got["deployment"] != "d1" || got["step.name"] != "qa" {
		t.Errorf("structured data inattesi: %v", got)
	}
}
