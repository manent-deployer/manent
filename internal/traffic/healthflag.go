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

// Impostazioni del provider health-flag.
const (
	settingMode          = "mode"                // http (default) o file
	settingDrainURL      = "drain_url"           // http: URL che mette il target in svuotamento ({host}, {port}, {label.<nome>})
	settingDrainMethod   = "drain_method"        // http: metodo per svuotare (default PUT)
	settingRestoreMethod = "restore_method"      // http: metodo per rientrare, sullo stesso URL (default DELETE)
	settingTokenSecret   = "token_secret"        // http: segreto con il bearer token
	settingFlagPath      = "path"                // file: file flag sul target, via SSH
	settingSSHUser       = "ssh_user"            // file: utente SSH (default: quello del target)
	settingSSHKey        = "ssh_key_secret"      // file: segreto con la chiave privata
	settingSSHPassword   = "ssh_password_secret" // file: in alternativa, segreto con la password
	settingKnownHosts    = "known_hosts"         // file: known_hosts (default ~/.ssh/known_hosts)
	settingHealthURL     = "health_url"          // sonda dello stato di salute visto dal load balancer
	settingDrainDelay    = "drain_delay"         // attesa perché il load balancer veda il target fuori servizio (default 30s)
	defaultDrainDelay    = 30 * time.Second
)

// healthFlagPool funziona con qualunque load balancer: il target risponde 503 al proprio
// health check (per una chiamata HTTP o per un file flag) e il load balancer smette di
// mandargli traffico. Non conta le connessioni: si attende drain_delay.
type healthFlagPool struct {
	s       map[string]string
	targets map[string]descriptor.Target
	client  *http.Client
	dialer  Dialer
	secrets func(context.Context, string) (string, error)
	delay   time.Duration
}

// Dialer apre le sessioni SSH verso i target.
type Dialer = customstep.Dialer

func validateHealthFlag(s map[string]string) error {
	var errs []error
	switch s[settingMode] {
	case "", "http":
		if !strings.Contains(s[settingDrainURL], "{host}") {
			errs = append(errs, fmt.Errorf("%s obbligatorio, con {host}", settingDrainURL))
		} else if err := checkURL(s, settingDrainURL, s[settingDrainURL]); err != nil {
			errs = append(errs, err)
		}
	case "file":
		if p := s[settingFlagPath]; !path.IsAbs(p) || strings.ContainsAny(p, "'\"$`\\ \n") {
			errs = append(errs, fmt.Errorf("%s deve essere un percorso assoluto senza spazi né caratteri speciali", settingFlagPath))
		}
		if s[settingSSHKey] == "" && s[settingSSHPassword] == "" {
			errs = append(errs, fmt.Errorf("%s o %s obbligatorio", settingSSHKey, settingSSHPassword))
		}
	default:
		errs = append(errs, fmt.Errorf("%s %q non valido (http|file)", settingMode, s[settingMode]))
	}
	if u := s[settingHealthURL]; u != "" && !strings.Contains(u, "://{host}") {
		errs = append(errs, fmt.Errorf("%s deve essere http(s)://{host}...", settingHealthURL))
	}
	if d := s[settingDrainDelay]; d != "" {
		if v, err := time.ParseDuration(d); err != nil || v < 0 || v > time.Hour {
			errs = append(errs, fmt.Errorf("%s %q non valido", settingDrainDelay, d))
		}
	}
	return errors.Join(errs...)
}

// NewHealthFlagPool costruisce il pool health-flag.
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

// DrainDelay implementa DrainDelayer.
func (p *healthFlagPool) DrainDelay() time.Duration { return p.delay }

// Members implementa Pool: il provider non conosce gli altri membri del pool.
func (p *healthFlagPool) Members(context.Context) ([]Member, error) { return nil, nil }

// Connections implementa Pool: le connessioni non sono note, l'attesa è drain_delay.
func (p *healthFlagPool) Connections(_ context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	return out, nil
}

// Drain implementa Pool.
func (p *healthFlagPool) Drain(ctx context.Context, members []string) error {
	return p.each(ctx, members, true)
}

// Restore implementa Pool.
func (p *healthFlagPool) Restore(ctx context.Context, members []string) error {
	return p.each(ctx, members, false)
}

func (p *healthFlagPool) each(ctx context.Context, members []string, drain bool) error {
	for _, m := range members {
		t, ok := p.targets[m]
		if !ok {
			return fmt.Errorf("membro %s non corrisponde ad alcun target", m)
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
	// Rientrare da un target mai svuotato non è un errore.
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
		return fmt.Errorf("%s: uscita %d: %s", cmd, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Healthy implementa Pool: senza health_url un target rientrato è considerato sano.
func (p *healthFlagPool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range members {
		t, ok := p.targets[m]
		if !ok {
			return nil, fmt.Errorf("membro %s non corrisponde ad alcun target", m)
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

// expandURL sostituisce i dati del target in un URL, codificandoli.
func expandURL(tmpl string, t descriptor.Target) string {
	esc := t
	esc.Labels = map[string]string{}
	for k, v := range t.Labels {
		esc.Labels[k] = url.PathEscape(v)
	}
	return descriptor.ExpandTarget(tmpl, esc)
}
