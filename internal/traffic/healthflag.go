package traffic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// Settings of the health-flag provider.
const (
	settingMode          = "mode"                // http (default) or file
	settingDrainURL      = "drain_url"           // http: URL that puts the target into draining ({host}, {port}, {label.<name>})
	settingDrainMethod   = "drain_method"        // http: method to drain (default PUT)
	settingRestoreMethod = "restore_method"      // http: method to restore, on the same URL (default DELETE)
	settingTokenSecret   = "token_secret"        // http: secret with the bearer token
	settingFlagPath      = "path"                // file: flag file on the target, over SSH
	settingSSHUser       = "ssh_user"            // file: SSH user (default: the target's)
	settingSSHKey        = "ssh_key_secret"      // file: secret with the private key
	settingSSHPassword   = "ssh_password_secret" // file: alternatively, secret with the password
	settingKnownHosts    = "known_hosts"         // file: known_hosts (default ~/.ssh/known_hosts)
	settingHealthURL     = "health_url"          // probe of the health state seen by the load balancer
	settingDrainDelay    = "drain_delay"         // wait for the load balancer to see the target out of service (default 30s)
	defaultDrainDelay    = 30 * time.Second
)

// healthFlagPool works with any load balancer: the target answers 503 to its own health check
// (through an HTTP call or a flag file) and the load balancer stops sending it traffic. It
// does not count connections: it waits drain_delay.
type healthFlagPool struct {
	s       map[string]string
	targets map[string]descriptor.Target
	client  *http.Client
	dialer  Dialer
	secrets func(context.Context, string) (string, error)
	delay   time.Duration
}

// Dialer opens the SSH sessions to the targets.
type Dialer = customstep.Dialer

func validateHealthFlag(s map[string]string) error {
	var errs []error
	switch s[settingMode] {
	case "", "http":
		if !strings.Contains(s[settingDrainURL], "{host}") {
			errs = append(errs, fmt.Errorf("%s is required, with {host}", settingDrainURL))
		} else if err := checkURL(s, settingDrainURL, s[settingDrainURL]); err != nil {
			errs = append(errs, err)
		}
	case "file":
		if p := s[settingFlagPath]; !path.IsAbs(p) || strings.ContainsAny(p, "'\"$`\\ \n") {
			errs = append(errs, fmt.Errorf("%s must be an absolute path without spaces or special characters", settingFlagPath))
		}
		if s[settingSSHKey] == "" && s[settingSSHPassword] == "" {
			errs = append(errs, fmt.Errorf("%s or %s is required", settingSSHKey, settingSSHPassword))
		}
	default:
		errs = append(errs, fmt.Errorf("invalid %s %q (http|file)", settingMode, s[settingMode]))
	}
	if u := s[settingHealthURL]; u != "" && !strings.Contains(u, "://{host}") {
		errs = append(errs, fmt.Errorf("%s must be http(s)://{host}...", settingHealthURL))
	}
	if d := s[settingDrainDelay]; d != "" {
		if v, err := time.ParseDuration(d); err != nil || v < 0 || v > time.Hour {
			errs = append(errs, fmt.Errorf("invalid %s %q", settingDrainDelay, d))
		}
	}
	return errors.Join(errs...)
}

// NewHealthFlagPool builds the health-flag pool.
func NewHealthFlagPool(_ context.Context, env Env) (Pool, error) {
	if err := validateHealthFlag(env.Settings); err != nil {
		return nil, err
	}
	c, err := httpClient(env.Settings, env.Timeout)
	if err != nil {
		return nil, err
	}
	p := &healthFlagPool{s: env.Settings, targets: map[string]descriptor.Target{}, client: c, dialer: env.Dialer, delay: defaultDrainDelay}
	if env.Secrets != nil {
		p.secrets = env.Secrets.Secret
	}
	if d := env.Settings[settingDrainDelay]; d != "" {
		p.delay, _ = time.ParseDuration(d)
	}
	if p.dialer == nil {
		p.dialer = &customstep.SSHDialer{KnownHostsPath: env.Settings[settingKnownHosts]}
	}
	for _, t := range env.Targets {
		p.targets[env.Member(t)] = t
	}
	return p, nil
}

// DrainDelay implements DrainDelayer.
func (p *healthFlagPool) DrainDelay() time.Duration { return p.delay }

// Members implements Pool: the provider does not know the other pool members.
func (p *healthFlagPool) Members(context.Context) ([]Member, error) { return nil, nil }

// Connections implements Pool: connections are unknown, the wait is drain_delay.
func (p *healthFlagPool) Connections(_ context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	return out, nil
}

// Drain implements Pool.
func (p *healthFlagPool) Drain(ctx context.Context, members []string) error {
	return p.each(ctx, members, true)
}

// Restore implements Pool.
func (p *healthFlagPool) Restore(ctx context.Context, members []string) error {
	return p.each(ctx, members, false)
}

func (p *healthFlagPool) each(ctx context.Context, members []string, drain bool) error {
	for _, m := range members {
		t, ok := p.targets[m]
		if !ok {
			return fmt.Errorf("member %s matches no target", m)
		}
		var err error
		if p.s[settingMode] == "file" {
			err = p.flagFile(ctx, t, drain)
		} else {
			err = p.flagHTTP(ctx, t, drain)
		}
		if err != nil {
			return fmt.Errorf("target %s: %w", t.Host, err)
		}
	}
	return nil
}

func (p *healthFlagPool) flagHTTP(ctx context.Context, t descriptor.Target, drain bool) error {
	method := strings.ToUpper(p.s[settingDrainMethod])
	if method == "" {
		method = http.MethodPut
	}
	if !drain {
		if method = strings.ToUpper(p.s[settingRestoreMethod]); method == "" {
			method = http.MethodDelete
		}
	}
	token := ""
	if name := p.s[settingTokenSecret]; name != "" {
		var err error
		if token, err = p.secrets(ctx, name); err != nil {
			return err
		}
	}
	u := expandURL(p.s[settingDrainURL], t)
	err := doJSON(ctx, p.client, method, u, nil, nil, func(r *http.Request) {
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
		}
	})
	// Restoring a target never drained is not an error.
	if se, ok := errors.AsType[*StatusError](err); ok && !drain && se.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (p *healthFlagPool) flagFile(ctx context.Context, t descriptor.Target, drain bool) error {
	var auth customstep.SSHAuth
	var err error
	if name := p.s[settingSSHKey]; name != "" {
		auth.PrivateKey, err = p.secrets(ctx, name)
	} else {
		auth.Password, err = p.secrets(ctx, p.s[settingSSHPassword])
	}
	if err != nil {
		return err
	}
	user := p.s[settingSSHUser]
	if user == "" {
		user = t.User
	}
	conn, err := p.dialer.Dial(ctx, t, user, auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	cmd := "rm -f '" + p.s[settingFlagPath] + "'"
	if drain {
		cmd = "touch '" + p.s[settingFlagPath] + "'"
	}
	var stderr bytes.Buffer
	code, err := conn.Exec(ctx, cmd, nil, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s: exit %d: %s", cmd, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Healthy implements Pool: without health_url a restored target counts as healthy.
func (p *healthFlagPool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range members {
		t, ok := p.targets[m]
		if !ok {
			return nil, fmt.Errorf("member %s matches no target", m)
		}
		if p.s[settingHealthURL] == "" {
			out[m] = true
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, expandURL(p.s[settingHealthURL], t), nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			out[m] = false
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		out[m] = resp.StatusCode == http.StatusOK
	}
	return out, nil
}

// expandURL replaces the target data in a URL, encoding it.
func expandURL(tmpl string, t descriptor.Target) string {
	esc := t
	esc.Labels = map[string]string{}
	for k, v := range t.Labels {
		esc.Labels[k] = url.PathEscape(v)
	}
	return descriptor.ExpandTarget(tmpl, esc)
}
