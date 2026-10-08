// Package logging fornisce il logger applicativo e l'inoltro dei log al SIEM in formato RFC 5424.
package logging

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/crewjam/rfc5424"
)

// SIEMSender invia un messaggio RFC 5424 al SIEM.
type SIEMSender interface {
	Send(msg rfc5424.Message) error
	Close() error
}

// NoopSender scarta i messaggi; usato quando il SIEM è disabilitato.
type NoopSender struct{}

// Send non fa nulla.
func (NoopSender) Send(rfc5424.Message) error { return nil }

// Close non fa nulla.
func (NoopSender) Close() error { return nil }

// NetSender invia i messaggi via UDP (un datagramma per messaggio) o TCP (octet counting, RFC 6587).
type NetSender struct {
	network string
	address string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewNetSender crea un sender di rete; la connessione è aperta alla prima Send.
func NewNetSender(network, address string) (*NetSender, error) {
	if network != "udp" && network != "tcp" {
		return nil, fmt.Errorf("rete SIEM %q non supportata (udp|tcp)", network)
	}
	if address == "" {
		return nil, fmt.Errorf("indirizzo SIEM obbligatorio")
	}
	return &NetSender{network: network, address: address, timeout: 5 * time.Second}, nil
}

// Send serializza e invia il messaggio, riaprendo la connessione in caso di errore.
func (s *NetSender) Send(msg rfc5424.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		c, err := net.DialTimeout(s.network, s.address, s.timeout)
		if err != nil {
			return err
		}
		s.conn = c
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	var err error
	if s.network == "tcp" {
		_, err = msg.WriteTo(s.conn)
	} else {
		var b []byte
		if b, err = msg.MarshalBinary(); err == nil {
			_, err = s.conn.Write(b)
		}
	}
	if err != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	return err
}

// Close chiude la connessione aperta.
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
