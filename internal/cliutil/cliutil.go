// Package cliutil holds helpers shared by the client and server CLIs. It must not import
// heavy packages: it is linked into the client too.
package cliutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jessevdk/go-flags"

	"github.com/manent-deployer/manent/internal/version"
)

// PrintJSON prints v to stdout as indented JSON.
func PrintJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// SignalContext is cancelled on SIGINT or SIGTERM.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// VersionCmd shows the binary version.
type VersionCmd struct{}

// Execute implements flags.Commander.
func (c *VersionCmd) Execute([]string) error { return PrintJSON(version.Get()) }

// Parse runs the parser; a help request is not an error. A missing command (e.g. "manent" or
// "manent policy" alone) prints the full help of the command reached, like --help, instead of
// the bare list of command names, and still fails.
func Parse(p *flags.Parser, args []string) error {
	printErrors := p.Options&flags.PrintErrors != 0
	p.Options &^= flags.PrintErrors
	_, err := p.ParseArgs(args)
	var ferr *flags.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ferr) && ferr.Type == flags.ErrHelp:
		if printErrors {
			fmt.Fprintln(os.Stdout, err)
		}
		return nil
	case errors.As(err, &ferr) && ferr.Type == flags.ErrCommandRequired:
		p.WriteHelp(os.Stderr)
		return err
	}
	if printErrors {
		fmt.Fprintln(os.Stderr, err)
	}
	return err
}

// Command describes a subcommand to register.
type Command struct {
	Name, Short string
	Cmd         any
}

// AddGroup registers a command with mandatory subcommands.
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
