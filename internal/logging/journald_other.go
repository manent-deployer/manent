//go:build !unix

package logging

import "errors"

// JournalSender non è disponibile fuori dai sistemi Unix.
type JournalSender struct{ NoopSender }

// NewJournalSender restituisce sempre un errore: journald esiste solo su Linux.
func NewJournalSender(string) (*JournalSender, error) {
	return nil, errors.New("rete SIEM journald non supportata su questo sistema")
}
