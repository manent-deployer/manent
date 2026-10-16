//go:build !unix

package logging

import "errors"

// JournalSender is not available outside Unix systems.
type JournalSender struct{ NoopSender }

// NewJournalSender always returns an error: journald exists only on Linux.
func NewJournalSender(string) (*JournalSender, error) {
	return nil, errors.New("rete SIEM journald non supportata su questo sistema")
}
