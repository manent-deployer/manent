//go:build unix

package logging

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewjam/rfc5424"
)

// listenUnixgram opens a datagram socket in a short directory (unix socket paths have a limit
// of about 100 characters).
func listenUnixgram(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mlog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	return listenAt(t, path), path
}

func listenAt(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// receive reads a datagram; if it carries a file descriptor it returns the file content.
func receive(t *testing.T, c *net.UnixConn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf, oob := make([]byte, 1<<20), make([]byte, 1024)
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	if oobn == 0 {
		return buf[:n]
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messaggio di controllo: %v", err)
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("descrittori: %v", err)
	}
	f := os.NewFile(uintptr(fds[0]), "journal")
	defer f.Close()
	st, _ := f.Stat()
	data := make([]byte, st.Size())
	if _, err := f.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	return data
}

// parseJournal decodes the native journald protocol.
func parseJournal(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.Fatalf("campo senza terminatore: %q", data)
		}
		line := string(data[:i])
		data = data[i+1:]
		if name, value, ok := strings.Cut(line, "="); ok {
			out[name] = append(out[name], value)
			continue
		}
		n := binary.LittleEndian.Uint64(data[:8])
		out[line] = append(out[line], string(data[8:8+n]))
		data = data[8+n+1:]
	}
	return out
}

func testMessage(text string) rfc5424.Message {
	return rfc5424.Message{
		Priority: facilityLocal0 | rfc5424.Warning, Timestamp: time.Now(), Hostname: "h1",
		AppName: "manent", ProcessID: "42", MessageID: "-", Message: []byte(text),
		StructuredData: []rfc5424.StructuredData{{ID: sdID, Parameters: []rfc5424.SDParam{
			{Name: "deployment", Value: "d1"}, {Name: "step.name", Value: "qa"},
		}}},
	}
}

func TestSyslogLocale(t *testing.T) {
	srv, path := listenUnixgram(t)
	s, err := NewSender(NetworkUnixgram, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send(testMessage("step fallito")); err != nil {
		t.Fatal(err)
	}
	got := string(receive(t, srv))
	if !strings.HasPrefix(got, "<132>1 ") || !strings.Contains(got, `[manent@32473 deployment="d1" step.name="qa"]`) || !strings.HasSuffix(got, "step fallito") {
		t.Fatalf("messaggio RFC 5424 inatteso: %q", got)
	}
	// The daemon restarts and recreates the socket: the next message still arrives.
	_ = srv.Close()
	_ = os.Remove(path)
	srv = listenAt(t, path)
	if err := s.Send(testMessage("dopo il riavvio")); err != nil {
		t.Fatalf("invio dopo la ricreazione del socket: %v", err)
	}
	if got := string(receive(t, srv)); !strings.HasSuffix(got, "dopo il riavvio") {
		t.Fatalf("messaggio dopo il riavvio: %q", got)
	}
}

func TestJournaldNativo(t *testing.T) {
	srv, path := listenUnixgram(t)
	s, err := NewSender(NetworkJournald, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send(testMessage("riga 1\nriga 2")); err != nil {
		t.Fatal(err)
	}
	f := parseJournal(t, receive(t, srv))
	want := map[string]string{
		"MESSAGE": "riga 1\nriga 2", "PRIORITY": "4", "SYSLOG_FACILITY": "16", "SYSLOG_IDENTIFIER": "manent",
		"SYSLOG_PID": "42", "MANENT_DEPLOYMENT": "d1", "MANENT_STEP_NAME": "qa",
	}
	for k, v := range want {
		if len(f[k]) != 1 || f[k][0] != v {
			t.Errorf("%s = %q, atteso %q", k, f[k], v)
		}
	}
}

// A message larger than a datagram goes through a temporary file's descriptor.
func TestJournaldMessaggioGrande(t *testing.T) {
	srv, path := listenUnixgram(t)
	s, err := NewJournalSender(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := strings.Repeat("x", 300*1024)
	if err := s.Send(testMessage(big)); err != nil {
		t.Fatal(err)
	}
	if f := parseJournal(t, receive(t, srv)); len(f["MESSAGE"]) != 1 || f["MESSAGE"][0] != big {
		t.Fatalf("messaggio grande non ricevuto per intero (%d campi)", len(f))
	}
}

func TestJournalName(t *testing.T) {
	for in, want := range map[string]string{
		"deployment": "MANENT_DEPLOYMENT", "step.name": "MANENT_STEP_NAME", "dry-run": "MANENT_DRY_RUN",
		strings.Repeat("a", 80): "MANENT_" + strings.Repeat("A", 57),
	} {
		if got := journalName(in); got != want {
			t.Errorf("journalName(%q) = %q, atteso %q", in, got, want)
		}
	}
}

// Several destinations: an error on one does not prevent sending to the others.
func TestMultiSender(t *testing.T) {
	srv, path := listenUnixgram(t)
	ok, _ := NewSender(NetworkUnixgram, path)
	broken, _ := NewSender(NetworkUnixgram, filepath.Join(filepath.Dir(path), "assente"))
	m := MultiSender{broken, ok}
	defer m.Close()
	if err := m.Send(testMessage("a tutti")); err == nil {
		t.Error("errore della destinazione assente non riportato")
	}
	if got := string(receive(t, srv)); !strings.HasSuffix(got, "a tutti") {
		t.Fatalf("destinazione funzionante: %q", got)
	}
}
