package traffic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/manent-deployer/manent/internal/customstep"
	"github.com/manent-deployer/manent/internal/descriptor"
)

// Settings of the nginx provider (open source NGINX over SSH).
const (
	settingLBHosts       = "lb_hosts"       // hosts of the NGINX instances, comma separated
	settingUpstreamFile  = "upstream_file"  // file included in the upstream, dedicated to the servers managed by manent
	settingReloadCommand = "reload_command" // check and reload command (default "nginx -t && nginx -s reload")
	settingSSHPort       = "ssh_port"       // SSH port of the instances (default 22)
	defaultReloadCommand = "nginx -t && nginx -s reload"
)

// serverLineRe matches a server directive: address and parameters.
var serverLineRe = regexp.MustCompile(`^(\s*server\s+)(\S+)([^;]*);(.*)$`)

// nginxPool drains the servers of an open source NGINX upstream, which has no runtime API:
// manent rewrites over SSH a dedicated file included in the upstream (down parameter) and
// reloads NGINX on every instance. Connections are unknown: it waits drain_delay.
type nginxPool struct {
	s       map[string]string
	hosts   []string
	file    string
	reload  string
	port    int
	dialer  Dialer
	secrets func(context.Context, string) (string, error)
	delay   time.Duration
}

func validateNginx(s map[string]string) error {
	var errs []error
	if len(splitList(s[settingLBHosts])) == 0 {
		errs = append(errs, fmt.Errorf("%s is required", settingLBHosts))
	}
	if p := s[settingUpstreamFile]; !path.IsAbs(p) || strings.ContainsAny(p, "'\"$`\\ \n;&|") {
		errs = append(errs, fmt.Errorf("%s must be an absolute path without spaces or special characters", settingUpstreamFile))
	}
	if s[settingSSHKey] == "" && s[settingSSHPassword] == "" {
		errs = append(errs, fmt.Errorf("%s or %s is required", settingSSHKey, settingSSHPassword))
	}
	if p := s[settingSSHPort]; p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("invalid %s %q", settingSSHPort, p))
		}
	}
	if d := s[settingDrainDelay]; d != "" {
		if v, err := time.ParseDuration(d); err != nil || v < 0 || v > time.Hour {
			errs = append(errs, fmt.Errorf("invalid %s %q", settingDrainDelay, d))
		}
	}
	return errors.Join(errs...)
}

// NewNginxPool builds the nginx pool.
func NewNginxPool(_ context.Context, env Env) (Pool, error) {
	if err := validateNginx(env.Settings); err != nil {
		return nil, err
	}
	p := &nginxPool{s: env.Settings, hosts: splitList(env.Settings[settingLBHosts]), file: env.Settings[settingUpstreamFile],
		reload: env.Settings[settingReloadCommand], port: 22, dialer: env.Dialer, delay: defaultDrainDelay}
	if p.reload == "" {
		p.reload = defaultReloadCommand
	}
	if v := env.Settings[settingSSHPort]; v != "" {
		p.port, _ = strconv.Atoi(v)
	}
	if d := env.Settings[settingDrainDelay]; d != "" {
		p.delay, _ = time.ParseDuration(d)
	}
	if env.Secrets != nil {
		p.secrets = env.Secrets.Secret
	}
	if p.dialer == nil {
		p.dialer = &customstep.SSHDialer{KnownHostsPath: env.Settings[settingKnownHosts]}
	}
	return p, nil
}

// DrainDelay implements DrainDelayer.
func (p *nginxPool) DrainDelay() time.Duration { return p.delay }

func (p *nginxPool) conn(ctx context.Context, host string) (customstep.Conn, error) {
	var auth customstep.SSHAuth
	var err error
	if name := p.s[settingSSHKey]; name != "" {
		auth.PrivateKey, err = p.secrets(ctx, name)
	} else {
		auth.Password, err = p.secrets(ctx, p.s[settingSSHPassword])
	}
	if err != nil {
		return nil, err
	}
	return p.dialer.Dial(ctx, descriptor.Target{Host: host, Port: p.port}, p.s[settingSSHUser], auth)
}

func run(ctx context.Context, c customstep.Conn, cmd string, stdin io.Reader) (string, error) {
	var out, errb bytes.Buffer
	code, err := c.Exec(ctx, cmd, stdin, &out, &errb)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// nginxLine is a server directive of the file.
type nginxLine struct {
	addr string
	down bool
}

func parseUpstream(content string) []nginxLine {
	var out []nginxLine
	for _, l := range strings.Split(content, "\n") {
		m := serverLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		out = append(out, nginxLine{addr: m[2], down: hasParam(m[3], "down")})
	}
	return out
}

func hasParam(params, name string) bool {
	for _, f := range strings.Fields(params) {
		if f == name {
			return true
		}
	}
	return false
}

// setDown adds or removes the down parameter on the given servers; it returns the new content
// and the members not found.
func setDown(content string, members []string, down bool) (string, []string) {
	found := map[string]bool{}
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		m := serverLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		hit := false
		for _, mem := range members {
			if m[2] == mem {
				hit = true
				found[mem] = true
			}
		}
		if !hit {
			continue
		}
		var params []string
		for _, f := range strings.Fields(m[3]) {
			if f != "down" {
				params = append(params, f)
			}
		}
		if down {
			params = append(params, "down")
		}
		p := ""
		if len(params) > 0 {
			p = " " + strings.Join(params, " ")
		}
		lines[i] = m[1] + m[2] + p + ";" + m[4]
	}
	var missing []string
	for _, mem := range members {
		if !found[mem] {
			missing = append(missing, mem)
		}
	}
	return strings.Join(lines, "\n"), missing
}

func (p *nginxPool) read(ctx context.Context, c customstep.Conn) (string, error) {
	return run(ctx, c, "cat '"+p.file+"'", nil)
}

// edit rewrites the file on every instance (a copy first, then a rename) and reloads NGINX.
// If the file is already in the wanted state it does not reload.
func (p *nginxPool) edit(ctx context.Context, members []string, down bool) error {
	for _, h := range p.hosts {
		if err := p.editHost(ctx, h, members, down); err != nil {
			return fmt.Errorf("instance %s: %w", h, err)
		}
	}
	return nil
}

func (p *nginxPool) editHost(ctx context.Context, host string, members []string, down bool) error {
	c, err := p.conn(ctx, host)
	if err != nil {
		return err
	}
	defer c.Close()
	cur, err := p.read(ctx, c)
	if err != nil {
		return err
	}
	next, missing := setDown(cur, members, down)
	if len(missing) > 0 {
		return fmt.Errorf("servers missing from %s: %s", p.file, strings.Join(missing, ", "))
	}
	if next == cur {
		return nil
	}
	tmp := p.file + ".manent"
	if _, err := run(ctx, c, "cat > '"+tmp+"' && mv '"+tmp+"' '"+p.file+"' && ("+p.reload+")", strings.NewReader(next)); err != nil {
		return fmt.Errorf("update and reload: %w", err)
	}
	return nil
}

// Members implements Pool: the servers in the first instance's file; a down server is out of
// service. Open source NGINX does not expose server health.
func (p *nginxPool) Members(ctx context.Context) ([]Member, error) {
	c, err := p.conn(ctx, p.hosts[0])
	if err != nil {
		return nil, err
	}
	defer c.Close()
	cur, err := p.read(ctx, c)
	if err != nil {
		return nil, err
	}
	var out []Member
	for _, l := range parseUpstream(cur) {
		m := Member{Name: l.addr, State: StateActive, Healthy: true}
		if l.down {
			m.State = StateDisabled
		}
		out = append(out, m)
	}
	return out, nil
}

// Drain implements Pool.
func (p *nginxPool) Drain(ctx context.Context, members []string) error {
	return p.edit(ctx, members, true)
}

// Restore implements Pool.
func (p *nginxPool) Restore(ctx context.Context, members []string) error {
	return p.edit(ctx, members, false)
}

// Connections implements Pool: unknown, the wait is drain_delay.
func (p *nginxPool) Connections(_ context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	return out, nil
}

// Healthy implements Pool: a server present and not down on every instance.
func (p *nginxPool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range members {
		out[m] = true
	}
	for _, h := range p.hosts {
		c, err := p.conn(ctx, h)
		if err != nil {
			return nil, err
		}
		cur, err := p.read(ctx, c)
		c.Close()
		if err != nil {
			return nil, err
		}
		lines := parseUpstream(cur)
		for _, m := range members {
			ok := false
			for _, l := range lines {
				if l.addr == m && !l.down {
					ok = true
				}
			}
			out[m] = out[m] && ok
		}
	}
	return out, nil
}
