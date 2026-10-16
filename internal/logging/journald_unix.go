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

// journalInline is the size beyond which the message goes through a file: unix datagrams have
// a limit (SO_SNDBUF on Linux, much lower on macOS).
const journalInline = 128 * 1024

// JournalSender sends messages to systemd-journald with the native protocol
// (https://systemd.io/JOURNAL_NATIVE_PROTOCOL/): every structured data parameter becomes a
// MANENT_<NAME> field, filterable with journalctl (e.g.
// journalctl -t manent MANENT_DEPLOYMENT=<id>).
type JournalSender struct {
	path string

	mu   sync.Mutex
	conn *net.UnixConn
}

// NewJournalSender creates the sender; the socket is opened at the first send.
func NewJournalSender(path string) (*JournalSender, error) {
	if path == "" {
		return nil, errors.New("socket di journald obbligatorio")
	}
	return &JournalSender{path: path}, nil
}

// Send sends the message. A message too large for a datagram is written to a temporary,
// already removed file whose descriptor is passed (like sd_journal_send). An error reopens
// the socket and retries once: journald may have been restarted.
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

// sendFile passes the message through the descriptor of a temporary file.
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
	// Go does not allow WriteMsgUnix on a connected datagram socket: direct sendmsg.
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

// Close closes the socket.
func (s *JournalSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
	return nil
}

// journalFields serializes the message in the native journald protocol: "NAME=value\n", or,
// if the value contains a newline, "NAME\n" + length (64-bit little endian) + value + "\n".
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

// journalName converts an attribute name into a journal field: "MANENT_" followed by the
// uppercase name, with every character other than letters and digits replaced by "_" (e.g.
// step.name → MANENT_STEP_NAME), at most 64 characters.
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
