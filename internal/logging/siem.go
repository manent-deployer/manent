// Package logging provides the application logger and log forwarding to the SIEM in RFC 5424 format.
package logging

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/crewjam/rfc5424"
)

// SIEMSender sends an RFC 5424 message to the SIEM.
type SIEMSender interface {
	Send(msg rfc5424.Message) error
	Close() error
}

// NoopSender discards messages; used when the SIEM is disabled.
type NoopSender struct{}

// Send does nothing.
func (NoopSender) Send(rfc5424.Message) error { return nil }

// Close does nothing.
func (NoopSender) Close() error { return nil }

// Networks of the structured log destinations.
const (
	NetworkUDP = "udp"
	NetworkTCP = "tcp"
	// NetworkUnixgram is the local syslog socket (/dev/log): rsyslog, syslog-ng or journald.
	NetworkUnixgram = "unixgram"
	// NetworkJournald is the native systemd-journald protocol: attributes become journal fields.
	NetworkJournald = "journald"
)

// DefaultSyslogSocket is the system's local syslog socket.
func DefaultSyslogSocket() string {
	if runtime.GOOS == "darwin" {
		return "/var/run/syslog"
	}
	return "/dev/log"
}

// DefaultJournalSocket is the socket of the native journald protocol.
const DefaultJournalSocket = "/run/systemd/journal/socket"

// NewSender creates the sender for a destination. For unixgram and journald the address is
// the socket path, with a default if empty.
func NewSender(network, address string) (SIEMSender, error) {
	switch network {
	case NetworkJournald:
		if address == "" {
			address = DefaultJournalSocket
		}
		return NewJournalSender(address)
	case NetworkUnixgram:
		if runtime.GOOS == "windows" {
			return nil, errors.New("rete SIEM unixgram non supportata su Windows")
		}
		if address == "" {
			address = DefaultSyslogSocket()
		}
	}
	return NewNetSender(network, address)
}

// MultiSender sends every message to several destinations: an error on one does not stop the others.
type MultiSender []SIEMSender

// Send sends the message to all destinations.
func (m MultiSender) Send(msg rfc5424.Message) error {
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Send(msg))
	}
	return errors.Join(errs...)
}

// Close closes all destinations.
func (m MultiSender) Close() error {
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// NetSender sends messages over UDP or unix datagram sockets (one datagram per message) or
// TCP (octet counting, RFC 6587).
type NetSender struct {
	network string
	address string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewNetSender creates a network sender; the connection is opened at the first Send.
func NewNetSender(network, address string) (*NetSender, error) {
	if network != NetworkUDP && network != NetworkTCP && network != NetworkUnixgram {
		return nil, fmt.Errorf("rete SIEM %q non supportata (udp|tcp|unixgram|journald)", network)
	}
	if address == "" {
		return nil, fmt.Errorf("indirizzo SIEM obbligatorio")
	}
	return &NetSender{network: network, address: address, timeout: 5 * time.Second}, nil
}

// Send serializes and sends the message, reopening the connection on error. On a unix socket
// the message is retried once: the syslog daemon may have recreated the socket (e.g. when
// journald restarts) and a datagram never arrives half-way.
func (s *NetSender) Send(msg rfc5424.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.send(msg)
	if err != nil && s.network == NetworkUnixgram {
		err = s.send(msg)
	}
	return err
}

func (s *NetSender) send(msg rfc5424.Message) error {
	if s.conn == nil {
		c, err := net.DialTimeout(s.network, s.address, s.timeout)
		if err != nil {
			return err
		}
		s.conn = c
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	var err error
	if s.network == NetworkTCP {
		_, err = msg.WriteTo(s.conn)
	} else {
		var b []byte
		if b, err = marshalDatagram(msg, maxDatagram); err == nil {
			_, err = s.conn.Write(b)
		}
	}
	if err != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	return err
}

// Close closes the open connection.
func (s *NetSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// maxDatagram is the maximum size of a message sent as a datagram: the UDP limit (65507
// bytes) with a margin; it also applies to /dev/log, where rsyslog and syslog-ng truncate long
// messages anyway.
const maxDatagram = 60 * 1024

// maxDatagramParam is the length attribute values are shortened to in a message too large
// for a datagram.
const maxDatagramParam = 1024

// marshalDatagram serializes the message in at most limit bytes: if larger it first shortens
// the long structured data values, then the text. A truncated message is not lost: a datagram
// beyond the limit would be rejected entirely.
func marshalDatagram(msg rfc5424.Message, limit int) ([]byte, error) {
	b, err := msg.MarshalBinary()
	if err != nil || len(b) <= limit {
		return b, err
	}
	sds := make([]rfc5424.StructuredData, len(msg.StructuredData))
	for i, sd := range msg.StructuredData {
		sds[i] = rfc5424.StructuredData{ID: sd.ID, Parameters: make([]rfc5424.SDParam, len(sd.Parameters))}
		for j, p := range sd.Parameters {
			if len(p.Value) > maxDatagramParam {
				p.Value = p.Value[:maxDatagramParam] + "…"
			}
			sds[i].Parameters[j] = p
		}
	}
	msg.StructuredData = sds
	if b, err = msg.MarshalBinary(); err != nil || len(b) <= limit {
		return b, err
	}
	const mark = "… [troncato]"
	keep := len(msg.Message) - (len(b) - limit) - len(mark)
	if keep < 0 {
		return nil, fmt.Errorf("messaggio di %d byte oltre il limite di un datagramma (%d)", len(b), limit)
	}
	msg.Message = append(append([]byte(nil), msg.Message[:keep]...), mark...)
	return msg.MarshalBinary()
}
