package windows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/manent-deployer/manent/internal/traffic"
	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/remotecopy"
)

// Traffic pools on Windows load balancers, driven with PowerShell on a management host over
// the same transports as the plugin:
//   - iis-arr: a web farm of IIS Application Request Routing; draining sets the ARR state of
//     the server to Drain (requests in progress and sticky sessions continue), restoring to
//     Available; the active connections are ARR's current requests.
//   - nlb: a Windows Network Load Balancing cluster; draining is Stop-NlbClusterNode -Drain
//     (it waits for the connections, up to drain_timeout), restoring Start-NlbClusterNode.
//
// Members are server addresses of the farm (iis-arr) or node names of the cluster (nlb).

// Pool provider names.
const (
	PoolARR = "iis-arr"
	PoolNLB = "nlb"
)

// Settings of the pools (traffic.pool.settings), besides the connection parameters of the
// plugin (transport, shell, winrm_*, password_secret, ssh_key_secret).
const (
	SettingHost         = "host"          // management host: the ARR server or a cluster node
	SettingUser         = "user"          // account on the management host
	SettingPort         = "port"          // SSH port (transport ssh)
	SettingFarm         = "farm"          // iis-arr: web farm name
	SettingDrainTimeout = "drain_timeout" // nlb: wait for the connections before stopping (default 10m)
	SettingCAFile       = "winrm_ca_file"
	SettingKnownHosts   = "known_hosts"
)

var memberRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

// ARR states of a farm server (SetState and the state counter).
const (
	arrAvailable = 0
	arrDrain     = 1
)

// PoolConnector opens the transport to the management host; replaced in tests.
type PoolConnector func(ctx context.Context, settings map[string]string, t plugin.Target, c Conn) (Transport, error)

// DefaultPoolConnector connects with DefaultConnector, configured from the settings.
func DefaultPoolConnector(ctx context.Context, settings map[string]string, t plugin.Target, c Conn) (Transport, error) {
	ca, err := readCA(settings[SettingCAFile])
	if err != nil {
		return nil, err
	}
	d := DefaultConnector{SSH: &remotecopy.SSHDialer{KnownHostsPath: settings[SettingKnownHosts]}, CACert: ca}
	return d.Connect(ctx, t, c)
}

// ValidateARR checks the settings of the iis-arr pool.
func ValidateARR(s map[string]string) error { return validatePool(PoolARR, s) }

// ValidateNLB checks the settings of the nlb pool.
func ValidateNLB(s map[string]string) error { return validatePool(PoolNLB, s) }

func validatePool(kind string, s map[string]string) error {
	_, err := poolConn(kind, s)
	return err
}

func poolConn(kind string, s map[string]string) (Conn, error) {
	var errs []error
	bad := func(k, why string) { errs = append(errs, fmt.Errorf("%s: %s", k, why)) }
	if !memberRe.MatchString(s[SettingHost]) {
		bad(SettingHost, "required: the management host")
	}
	if s[SettingUser] == "" || strings.ContainsAny(s[SettingUser], "\r\n") {
		bad(SettingUser, "required")
	}
	if kind == PoolARR && !nameRe.MatchString(s[SettingFarm]) {
		bad(SettingFarm, "required: the name of the ARR web farm")
	}
	if v := s[SettingDrainTimeout]; v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < time.Minute || d > 24*time.Hour {
			bad(SettingDrainTimeout, "duration between 1m and 24h")
		}
	}
	c := parseConn(s, s[ParamPassword], s[ParamSSHKey], bad)
	return c, errors.Join(errs...)
}

// NewPoolFactory returns the factory of the iis-arr or nlb pool.
func NewPoolFactory(kind string, connect PoolConnector) traffic.PoolFactory {
	return func(ctx context.Context, env traffic.Env) (traffic.Pool, error) {
		c, err := poolConn(kind, env.Settings)
		if err != nil {
			return nil, err
		}
		port := 0
		if c.Transport == TransportSSH {
			port = 22
			if v := env.Settings[SettingPort]; v != "" {
				if _, err := fmt.Sscanf(v, "%d", &port); err != nil {
					return nil, fmt.Errorf("%s: invalid port", SettingPort)
				}
			}
		}
		drain := 10 * time.Minute
		if v := env.Settings[SettingDrainTimeout]; v != "" {
			drain, _ = time.ParseDuration(v)
		}
		return &pool{kind: kind, settings: env.Settings, conn: c, connect: connect, secrets: env.Secrets,
			target: plugin.Target{Host: env.Settings[SettingHost], User: env.Settings[SettingUser], Port: port},
			farm:   env.Settings[SettingFarm], drain: drain}, nil
	}
}

type pool struct {
	kind     string
	settings map[string]string
	conn     Conn
	connect  PoolConnector
	secrets  plugin.SecretResolver
	target   plugin.Target
	farm     string
	drain    time.Duration
}

// member is the state of a server or node as written by the scripts.
type member struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Healthy  bool   `json:"healthy"`
	Enabled  bool   `json:"enabled"`
	Requests int64  `json:"requests"`
}

// run opens the transport (secrets resolved only now), runs the script and decodes the
// members it writes as JSON.
func (p *pool) run(ctx context.Context, script string) ([]member, error) {
	c := p.conn
	var err error
	if name := p.settings[ParamPassword]; name != "" {
		if c.Password, err = p.secrets.Secret(ctx, name); err != nil {
			return nil, err
		}
	}
	if name := p.settings[ParamSSHKey]; name != "" {
		if c.PrivateKey, err = p.secrets.Secret(ctx, name); err != nil {
			return nil, err
		}
	}
	tr, err := p.connect(ctx, p.settings, p.target, c)
	if err != nil {
		return nil, fmt.Errorf("%s: connection to %s: %w", p.kind, p.target.Host, err)
	}
	defer tr.Close()
	out, err := tr.Run(ctx, script, nil)
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %w", p.kind, p.target.Host, err)
	}
	var list []member
	if raw := results(out)["members"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, fmt.Errorf("%s: unreadable state: %w", p.kind, err)
		}
	}
	return list, nil
}

// names is the PowerShell array of the members, checked before entering the script.
func names(members []string) (string, error) {
	q := make([]string, len(members))
	for i, m := range members {
		if !memberRe.MatchString(m) {
			return "", fmt.Errorf("invalid member %q", m)
		}
		q[i] = psQuote(m)
	}
	return "$members = @(" + strings.Join(q, ", ") + ")\n", nil
}

// arrHead loads Microsoft.Web.Administration (Windows PowerShell 5.1 and PowerShell 7) and
// finds the farm; Servers lists the servers with ARR state, health and current requests.
const arrHead = `Add-Type -Path (Join-Path $env:windir 'System32\inetsrv\Microsoft.Web.Administration.dll')
$sm = New-Object Microsoft.Web.Administration.ServerManager
$farm = $sm.GetApplicationHostConfiguration().GetSection('webFarms').GetCollection() | Where-Object { $_.GetAttributeValue('name') -eq $farmName }
if ($null -eq $farm) { throw "web farm $farmName not found" }
function Servers {
  foreach ($s in $farm.GetCollection()) {
    $c = $s.GetChildElement('applicationRequestRouting').GetChildElement('counters')
    [pscustomobject]@{ name = [string]$s.GetAttributeValue('address'); state = [string][int]$c.GetAttributeValue('state');
      healthy = [bool]$c.GetAttributeValue('isHealthy'); enabled = [bool]$s.GetAttributeValue('enabled');
      requests = [long]$c.GetAttributeValue('currentRequests') }
  }
}
function Set-State($names, $state) {
  $missing = @($names | Where-Object { $n = $_; -not ($farm.GetCollection() | Where-Object { $_.GetAttributeValue('address') -eq $n }) })
  if ($missing.Count -gt 0) { throw ("servers missing from web farm " + $farmName + ": " + ($missing -join ', ')) }
  foreach ($s in $farm.GetCollection()) {
    if ($names -notcontains $s.GetAttributeValue('address')) { continue }
    $m = $s.GetChildElement('applicationRequestRouting').Methods['SetState'].CreateInstance()
    $m.Input.SetAttributeValue('newState', $state)
    $m.Execute()
  }
}
`

// nlbHead lists the cluster nodes with their state.
const nlbHead = `Import-Module NetworkLoadBalancingClusters
function Servers {
  foreach ($n in @(Get-NlbClusterNode)) {
    [pscustomobject]@{ name = [string]$n.Name; state = [string]$n.State; healthy = ([string]$n.State -eq 'Converged'); enabled = $true; requests = 0 }
  }
}
function Assert-Nodes($names) {
  $known = @(Servers | ForEach-Object { $_.name })
  $missing = @($names | Where-Object { $known -notcontains $_ })
  if ($missing.Count -gt 0) { throw ("nodes missing from the NLB cluster: " + ($missing -join ', ')) }
}
`

const writeMembers = "Write-Output ('MANENT:members=' + (ConvertTo-Json -Compress -InputObject @(Servers)))\n"

func (p *pool) head() string {
	if p.kind == PoolARR {
		return vars("farmName", p.farm) + arrHead
	}
	return nlbHead
}

// list reads the members.
func (p *pool) list(ctx context.Context) ([]member, error) {
	return p.run(ctx, p.head()+writeMembers)
}

func (p *pool) state(m member) traffic.Member {
	out := traffic.Member{Name: m.Name, Healthy: m.Healthy, State: traffic.StateActive}
	switch p.kind {
	case PoolARR:
		switch {
		case !m.Enabled:
			out.State = traffic.StateDisabled
		case m.State == fmt.Sprint(arrDrain):
			out.State = traffic.StateDraining
		case m.State != fmt.Sprint(arrAvailable):
			out.State = traffic.StateDisabled
		}
	default:
		switch m.State {
		case "Converged", "Converging":
		case "Draining":
			out.State = traffic.StateDraining
		default:
			out.State = traffic.StateDisabled
		}
	}
	return out
}

// Members implements traffic.Pool.
func (p *pool) Members(ctx context.Context) ([]traffic.Member, error) {
	list, err := p.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]traffic.Member, len(list))
	for i, m := range list {
		out[i] = p.state(m)
	}
	return out, nil
}

// change runs the drain or restore script on the members.
func (p *pool) change(ctx context.Context, members []string, drain bool) error {
	arr, err := names(members)
	if err != nil {
		return err
	}
	var body string
	switch {
	case p.kind == PoolARR && drain:
		body = fmt.Sprintf("Set-State $members %d\n", arrDrain)
	case p.kind == PoolARR:
		body = fmt.Sprintf("Set-State $members %d\n", arrAvailable)
	case drain:
		// Stop-NlbClusterNode -Drain returns when the connections are closed or, after the
		// timeout (minutes), stops the node anyway.
		mins := int(math.Ceil(p.drain.Minutes()))
		body = fmt.Sprintf("Assert-Nodes $members\nforeach ($m in $members) { Stop-NlbClusterNode -HostName $m -Drain -Timeout %d | Out-Null }\n", mins)
	default:
		body = "Assert-Nodes $members\nforeach ($m in $members) { Start-NlbClusterNode -HostName $m | Out-Null }\n"
	}
	_, err = p.run(ctx, p.head()+arr+body)
	return err
}

// Drain implements traffic.Pool.
func (p *pool) Drain(ctx context.Context, members []string) error {
	return p.change(ctx, members, true)
}

// Restore implements traffic.Pool.
func (p *pool) Restore(ctx context.Context, members []string) error {
	return p.change(ctx, members, false)
}

// selected returns the state of the given members; a missing one is an error.
func (p *pool) selected(ctx context.Context, members []string) (map[string]member, error) {
	list, err := p.list(ctx)
	if err != nil {
		return nil, err
	}
	by := map[string]member{}
	for _, m := range list {
		by[strings.ToLower(m.Name)] = m
	}
	out := map[string]member{}
	var missing []string
	for _, n := range members {
		m, ok := by[strings.ToLower(n)]
		if !ok {
			missing = append(missing, n)
			continue
		}
		out[n] = m
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: members missing: %s", p.kind, strings.Join(missing, ", "))
	}
	return out, nil
}

// Connections implements traffic.Pool: ARR's current requests; an NLB node counts one while
// it is still draining.
func (p *pool) Connections(ctx context.Context, members []string) (map[string]int, error) {
	sel, err := p.selected(ctx, members)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for n, m := range sel {
		switch {
		case p.kind == PoolARR:
			out[n] = int(m.Requests)
		case m.State == "Draining":
			out[n] = 1
		default:
			out[n] = 0
		}
	}
	return out, nil
}

// Healthy implements traffic.Pool.
func (p *pool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	sel, err := p.selected(ctx, members)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for n, m := range sel {
		out[n] = p.state(m).InService()
	}
	return out, nil
}

var _ traffic.Pool = (*pool)(nil)
