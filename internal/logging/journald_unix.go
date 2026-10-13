//go:build unix

package logging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/crewjam/rfc5424"
)

// journalInline è la dimensione oltre la quale il messaggio passa da un file: i datagrammi
// unix hanno un limite (SO_SNDBUF su Linux, molto più basso su macOS).
const journalInline = 128 * 1024

// JournalSender invia i messaggi a systemd-journald con il protocollo nativo
// (https://systemd.io/JOURNAL_NATIVE_PROTOCOL/): ogni parametro
// dello structured data diventa un campo MANENT_<NOME>, filtrabile con journalctl (es.
// journalctl -t manent MANENT_DEPLOYMENT=<id>).
type JournalSender struct {
	path string

	mu   sync.Mutex
	conn *net.UnixConn
}

// NewJournalSender crea il sender; il socket è aperto al primo invio.
func NewJournalSender(path string) (*JournalSender, error) {
	if path == "" {
		return nil, errors.New("socket di journald obbligatorio")
	}
	return &JournalSender{path: path}, nil
}

// Send invia il messaggio. Un messaggio troppo grande per un datagramma viene scritto in un
// file temporaneo, già rimosso, di cui si passa il descrittore (come sd_journal_send). Un errore
// riapre il socket e ritenta una volta: journald può essere stato riavviato.
func (s *JournalSender) Send(msg rfc5424.Message) error {
	data := journalFields(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.send(data)
	if err != nil {
		s.reset()
		err = s.send(data)
	}
	return err
}

func (s *JournalSender) send(data []byte) error {
	if s.conn == nil {
		c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: s.path, Net: "unixgram"})
		if err != nil {
			return err
		}
		s.conn = c
	}
	if len(data) <= journalInline {
		_, err := s.conn.Write(data)
		if !errors.Is(err, syscall.EMSGSIZE) && !errors.Is(err, syscall.ENOBUFS) {
			return err
		}
	}
	return s.sendFile(data)
}

// sendFile passa il messaggio tramite il descrittore di un file temporaneo.
func (s *JournalSender) sendFile(data []byte) error {
	f, err := os.CreateTemp("", "manent-journal-")
	if err != nil {
		return err
	}
	defer f.Close()
	_ = os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		return err
	}
	// Go non ammette WriteMsgUnix su un socket a datagrammi connesso: sendmsg diretto.
	raw, err := s.conn.SyscallConn()
	if err != nil {
		return err
	}
	rights := syscall.UnixRights(int(f.Fd()))
	var sendErr error
	if err := raw.Write(func(fd uintptr) bool {
		sendErr = syscall.Sendmsg(int(fd), nil, rights, nil, 0)
		return !errors.Is(sendErr, syscall.EAGAIN)
	}); err != nil {
		return err
	}
	return sendErr
}

func (s *JournalSender) reset() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// Close chiude il socket.
func (s *JournalSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
	return nil
}

// journalFields serializza il messaggio nel protocollo nativo di journald: "NOME=valore\n",
// oppure, se il valore contiene un a capo, "NOME\n" + lunghezza (64 bit little endian) + valore
// + "\n".
func journalFields(msg rfc5424.Message) []byte {
	var b bytes.Buffer
	add := func(name, value string) {
		if !strings.Contains(value, "\n") {
			b.WriteString(name + "=" + value + "\n")
			return
		}
		b.WriteString(name + "\n")
		_ = binary.Write(&b, binary.LittleEndian, uint64(len(value)))
		b.WriteString(value + "\n")
	}
	add("MESSAGE", string(msg.Message))
	add("PRIORITY", strconv.Itoa(int(msg.Priority&7)))
	add("SYSLOG_FACILITY", strconv.Itoa(int(msg.Priority>>3)))
	if msg.AppName != "" {
		add("SYSLOG_IDENTIFIER", msg.AppName)
	}
	if msg.ProcessID != "" {
		add("SYSLOG_PID", msg.ProcessID)
	}
	for _, sd := range msg.StructuredData {
		for _, p := range sd.Parameters {
			add(journalName(p.Name), p.Value)
		}
	}
	return b.Bytes()
}

// journalName converte il nome di un attributo in un campo del journal: "MANENT_" seguito dal
// nome in maiuscolo, con ogni carattere diverso da lettere e cifre sostituito da "_" (es.
// step.name → MANENT_STEP_NAME), al più 64 caratteri.
func journalName(s string) string {
	name := "MANENT_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, s)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

var _ SIEMSender = (*JournalSender)(nil)
