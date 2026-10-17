package clientcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// ReleaseSubmitCmd enqueues a composite release and, by default, follows its progress.
type ReleaseSubmitCmd struct {
	global   *GlobalOptions
	File     string        `short:"f" long:"file" required:"true" description:"release YAML (kind: Release); i file dei deploy sono relativi a questo"`
	NoWait   bool          `long:"no-wait" description:"non attendere la conclusione della release"`
	Timeout  time.Duration `long:"timeout" description:"tempo massimo di attesa, es. 2h (0 = nessun limite); la release prosegue comunque"`
	Interval time.Duration `long:"interval" default:"3s" description:"intervallo di lettura dello stato"`
}

// Execute implements flags.Commander.
func (c *ReleaseSubmitCmd) Execute([]string) error {
	r, err := descriptor.LoadReleaseYAMLFile(c.File)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		rs, err := svc.SubmitRelease(ctx, r)
		if err != nil {
			return err
		}
		if c.NoWait {
			return cliutil.PrintJSON(rs)
		}
		fmt.Fprintln(os.Stderr, "release accodata:", rs.ID, "(manent release status", rs.ID+")")
		if c.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.Timeout)
			defer cancel()
		}
		rs, err = followRelease(ctx, svc, rs, c.Interval, os.Stderr)
		if err != nil {
			return err
		}
		if err := cliutil.PrintJSON(rs); err != nil {
			return err
		}
		if rs.State != service.StateSuccess {
			return fmt.Errorf("release %s ended in %s: %s", rs.ID, rs.State, rs.Error)
		}
		return nil
	})
}

// followRelease reads the release state until it ends, writing to out every state change of a
// deploy or of its rollback.
func followRelease(ctx context.Context, svc service.Service, rs *service.ReleaseStatus, every time.Duration, out io.Writer) (*service.ReleaseStatus, error) {
	seen := map[string]string{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, d := range rs.Deploys {
			state := d.State
			if d.RollbackState != "" {
				state += " (rollback " + d.RollbackState + ")"
			}
			if seen[d.Name] != state {
				seen[d.Name] = state
				fmt.Fprintf(out, "  %-20s %s\n", d.Name, state)
			}
		}
		if service.Terminal(rs.State) {
			return rs, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("release %s still running: %w", rs.ID, ctx.Err())
		case <-t.C:
		}
		next, err := svc.ReleaseStatus(ctx, rs.ID)
		if err != nil {
			return nil, err
		}
		rs = next
	}
}

// ReleaseStatusCmd shows the state of a release.
type ReleaseStatusCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *ReleaseStatusCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		rs, err := svc.ReleaseStatus(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rs)
	})
}

// ReleaseListCmd lists the most recent releases.
type ReleaseListCmd struct {
	global *GlobalOptions
	Limit  int `short:"n" long:"limit" default:"20" description:"numero massimo di release"`
}

// Execute implements flags.Commander.
func (c *ReleaseListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		rs, err := svc.ListReleases(ctx, c.Limit)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rs)
	})
}

// ReleaseCancelCmd cancels a release.
type ReleaseCancelCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *ReleaseCancelCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.CancelRelease(ctx, c.Args.ID); err != nil {
			return err
		}
		fmt.Println("release annullata:", c.Args.ID)
		return nil
	})
}
