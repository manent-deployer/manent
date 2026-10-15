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

	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// Impostazioni del provider nginx (NGINX open source via SSH).
const (
	settingLBHosts       = "lb_hosts"       // host delle istanze di NGINX, separati da virgola
	settingUpstreamFile  = "upstream_file"  // file incluso nell'upstream, dedicato ai server gestiti da manent
	settingReloadCommand = "reload_command" // comando di verifica e ricarica (default "nginx -t && nginx -s reload")
	settingSSHPort       = "ssh_port"       // porta SSH delle istanze (default 22)
	defaultReloadCommand = "nginx -t && nginx -s reload"
)

// serverLineRe riconosce una direttiva server: indirizzo e parametri.
var serverLineRe = regexp.MustCompile(`^(\s*server\s+)(\S+)([^;]*);(.*)$`)

// nginxPool svuota i server di un upstream di NGINX open source, che non ha un'API a runtime:
// manent riscrive via SSH un file dedicato incluso nell'upstream (parametro down) e ricarica
// NGINX su ogni istanza. Le connessioni non sono note: si attende drain_delay.
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
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingLBHosts))
	}
	if p := s[settingUpstreamFile]; !path.IsAbs(p) || strings.ContainsAny(p, "'\"$`\\ \n;&|") {
		errs = append(errs, fmt.Errorf("%s deve essere un percorso assoluto senza spazi né caratteri speciali", settingUpstreamFile))
	}
	if s[settingSSHKey] == "" && s[settingSSHPassword] == "" {
		errs = append(errs, fmt.Errorf("%s o %s obbligatorio", settingSSHKey, settingSSHPassword))
	}
	if p := s[settingSSHPort]; p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s %q non valida", settingSSHPort, p))
		}
	}
	if d := s[settingDrainDelay]; d != "" {
		if v, err := time.ParseDuration(d); err != nil || v < 0 || v > time.Hour {
			errs = append(errs, fmt.Errorf("%s %q non valido", settingDrainDelay, d))
		}
	}
	return errors.Join(errs...)
}

// NewNginxPool costruisce il pool nginx.
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

// DrainDelay implementa DrainDelayer.
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
		return "", fmt.Errorf("uscita %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// nginxServer è una direttiva server del file.
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

// setDown aggiunge o toglie il parametro down ai server indicati; restituisce il nuovo
// contenuto e i membri non trovati.
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

// edit riscrive il file su ogni istanza (prima una copia, poi un rename) e ricarica NGINX. Se
// il file è già nello stato voluto non ricarica.
func (p *nginxPool) edit(ctx context.Context, members []string, down bool) error {
	for _, h := range p.hosts {
		if err := p.editHost(ctx, h, members, down); err != nil {
			return fmt.Errorf("istanza %s: %w", h, err)
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
		return fmt.Errorf("server assenti da %s: %s", p.file, strings.Join(missing, ", "))
	}
	if next == cur {
		return nil
	}
	tmp := p.file + ".manent"
	if _, err := run(ctx, c, "cat > '"+tmp+"' && mv '"+tmp+"' '"+p.file+"' && ("+p.reload+")", strings.NewReader(next)); err != nil {
		return fmt.Errorf("aggiornamento e ricarica: %w", err)
	}
	return nil
}

// Members implementa Pool: i server del file della prima istanza; un server down è fuori
// servizio. NGINX open source non espone la salute dei server.
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

// Drain implementa Pool.
func (p *nginxPool) Drain(ctx context.Context, members []string) error {
	return p.edit(ctx, members, true)
}

// Restore implementa Pool.
func (p *nginxPool) Restore(ctx context.Context, members []string) error {
	return p.edit(ctx, members, false)
}

// Connections implementa Pool: non note, l'attesa è drain_delay.
func (p *nginxPool) Connections(_ context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	return out, nil
}

// Healthy implementa Pool: un server presente e non down su ogni istanza.
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
