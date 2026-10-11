// Package cliutil contiene gli aiuti comuni alle CLI di client e server. Non deve importare
// package pesanti: è linkato anche nel client.
package cliutil

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/jessevdk/go-flags"

	"github.com/joeg-ita/manent/internal/version"
)

// PrintJSON stampa v su stdout in JSON indentato.
func PrintJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// SignalContext è cancellato alla ricezione di SIGINT o SIGTERM.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// VersionCmd mostra la versione del binario.
type VersionCmd struct{}

// Execute implementa flags.Commander.
func (c *VersionCmd) Execute([]string) error { return PrintJSON(version.Get()) }

// Parse esegue il parser; la richiesta di aiuto non è un errore.
func Parse(p *flags.Parser, args []string) error {
	_, err := p.ParseArgs(args)
	var ferr *flags.Error
	if errors.As(err, &ferr) && ferr.Type == flags.ErrHelp {
		return nil
	}
	return err
}

// Command descrive un sottocomando da registrare.
type Command struct {
	Name, Short string
	Cmd         any
}

// AddGroup registra un comando con sottocomandi obbligatori.
func AddGroup(p *flags.Parser, name, short, long string, cmds []Command) error {
	g, err := p.AddCommand(name, short, long, &struct{}{})
	if err != nil {
		return err
	}
	g.SubcommandsOptional = false
	for _, c := range cmds {
		if _, err := g.AddCommand(c.Name, c.Short, c.Short, c.Cmd); err != nil {
			return err
		}
	}
	return nil
}
