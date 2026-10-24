package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crewjam/rfc5424"
)

// fakeSender stores the received messages for checks.
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
	if m.Priority != facilityLocal0|rfc5424.Warning {
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

// A message too large for a datagram is shortened, not dropped.
func TestMessaggioDatagrammaAccorciato(t *testing.T) {
	msg := rfc5424.Message{
		Priority: facilityLocal0 | rfc5424.Info, Hostname: "h", AppName: "manent", ProcessID: "1", MessageID: "-",
		Message: []byte(strings.Repeat("m", 100*1024)),
		StructuredData: []rfc5424.StructuredData{{ID: sdID, Parameters: []rfc5424.SDParam{
			{Name: "deployment", Value: "d1"}, {Name: "pad", Value: strings.Repeat("p", 300*1024)},
		}}},
	}
	b, err := marshalDatagram(msg, maxDatagram)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxDatagram {
		t.Fatalf("datagramma di %d byte", len(b))
	}
	var got rfc5424.Message
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatalf("messaggio accorciato non valido: %v", err)
	}
	params := got.StructuredData[0].Parameters
	if params[0].Value != "d1" || len(params[1].Value) > maxDatagramParam+4 || !strings.HasSuffix(string(got.Message), "[truncated]") {
		t.Errorf("accorciamento inatteso: %d %q", len(params[1].Value), got.Message[len(got.Message)-20:])
	}
	// A small message stays identical.
	small := msg
	small.Message, small.StructuredData = []byte("ok"), nil
	if b, _ := marshalDatagram(small, maxDatagram); !strings.HasSuffix(string(b), " ok") {
		t.Errorf("messaggio piccolo modificato: %q", b)
	}
}
