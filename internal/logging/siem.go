// Package logging fornisce il logger applicativo e l'inoltro dei log al SIEM in formato RFC 5424.
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

// Reti delle destinazioni dei log strutturati.
const (
	NetworkUDP = "udp"
	NetworkTCP = "tcp"
	// NetworkUnixgram è il socket syslog locale (/dev/log): rsyslog, syslog-ng o journald.
	NetworkUnixgram = "unixgram"
	// NetworkJournald è il protocollo nativo di systemd-journald: gli attributi diventano
	// campi del journal.
	NetworkJournald = "journald"
)

// DefaultSyslogSocket è il socket syslog locale del sistema.
func DefaultSyslogSocket() string {
	if runtime.GOOS == "darwin" {
		return "/var/run/syslog"
	}
	return "/dev/log"
}

// DefaultJournalSocket è il socket del protocollo nativo di journald.
const DefaultJournalSocket = "/run/systemd/journal/socket"

// NewSender crea il sender per una destinazione. Per unixgram e journald l'indirizzo è il
// percorso del socket, con un default se vuoto.
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

// MultiSender invia ogni messaggio a più destinazioni: l'errore di una non ferma le altre.
type MultiSender []SIEMSender

// Send invia il messaggio a tutte le destinazioni.
func (m MultiSender) Send(msg rfc5424.Message) error {
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Send(msg))
	}
	return errors.Join(errs...)
}

// Close chiude tutte le destinazioni.
func (m MultiSender) Close() error {
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// NetSender invia i messaggi via UDP o socket unix a datagrammi (un datagramma per messaggio)
// o TCP (octet counting, RFC 6587).
type NetSender struct {
	network string
	address string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewNetSender crea un sender di rete; la connessione è aperta alla prima Send.
func NewNetSender(network, address string) (*NetSender, error) {
	if network != NetworkUDP && network != NetworkTCP && network != NetworkUnixgram {
		return nil, fmt.Errorf("rete SIEM %q non supportata (udp|tcp|unixgram|journald)", network)
	}
	if address == "" {
		return nil, fmt.Errorf("indirizzo SIEM obbligatorio")
	}
	return &NetSender{network: network, address: address, timeout: 5 * time.Second}, nil
}

// Send serializza e invia il messaggio, riaprendo la connessione in caso di errore. Su un socket
// unix il messaggio viene ritentato una volta: il demone syslog può aver ricreato il socket
// (es. al riavvio di journald) e un datagramma non arriva mai a metà.
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

// maxDatagram è la dimensione massima di un messaggio inviato come datagramma: il limite di UDP
// (65507 byte) con un margine; vale anche per /dev/log, dove rsyslog e syslog-ng troncano
// comunque i messaggi lunghi.
const maxDatagram = 60 * 1024

// maxDatagramParam è la lunghezza a cui vengono accorciati i valori degli attributi di un
// messaggio troppo grande per un datagramma.
const maxDatagramParam = 1024

// marshalDatagram serializza il messaggio in al più limit byte: se è più grande accorcia prima i
// valori lunghi dello structured data, poi il testo. Un messaggio troncato non va perso: un
// datagramma oltre il limite verrebbe rifiutato per intero.
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
