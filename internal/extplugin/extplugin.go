// Package extplugin starts and manages external deploy plugins: separate binaries that talk to
// manent over gRPC (pkg/plugin/external) and look like a normal plugin.Deployer to the rest of
// the engine. They are enabled with the "external" key in plugins.<name> of the configuration.
package extplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/pkg/plugin/external"
)

// Key is the key that, in plugins.<name>, identifies an external plugin.
const Key = "external"

// Config is the external section of a plugin.
type Config struct {
	// Command is the path of the plugin binary.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// SHA256 of the binary: verified at activation and at every process start.
	SHA256 string `yaml:"sha256"`
	// Env are the variables passed to the plugin. The worker's variables are not inherited,
	// except the system ones in inheritedEnv: they may contain secrets (e.g. env: references).
	Env map[string]string `yaml:"env"`
	// StartTimeout is the maximum time for the process to start (default 1m).
	StartTimeout time.Duration `yaml:"start_timeout"`
}

// inheritedEnv are the only worker variables passed to the plugin.
var inheritedEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG"}

// callTimeout limits Configure at process start.
const callTimeout = 30 * time.Second

// IsExternal reports whether a plugin configuration describes an external plugin.
func IsExternal(cfg map[string]any) bool {
	_, ok := cfg[Key]
	return ok
}

// NewFactory returns the factory of the external plugin registered with the given name.
// Activation checks configuration and binary but does not start the process: it is started
// on first use, so CLI commands (which activate plugins only to compute the plan) never run
// the binary.
func NewFactory(name string) plugin.Factory {
	return func(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
		c, err := parseConfig(cfg[Key])
		if err != nil {
			return nil, err
		}
		if err := c.checkBinary(); err != nil {
			return nil, err
		}
		rest := maps.Clone(cfg)
		delete(rest, Key)
		configJSON, err := json.Marshal(rest)
		if err != nil {
			return nil, fmt.Errorf("plugin configuration cannot be serialized to JSON: %w", err)
		}
		if c.SHA256 == "" {
			logger.Warn("external plugin without sha256: the binary is not verified", "command", c.Command)
		}
		return &Process{
			name:       name,
			cfg:        c,
			configJSON: configJSON,
			logger:     logger,
			hlog:       newHCLogger(name, logger),
		}, nil
	}
}

func parseConfig(raw any) (Config, error) {
	var c Config
	data, err := yaml.Marshal(raw)
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid section %s: %w", Key, err)
	}
	if c.Command == "" {
		return c, fmt.Errorf("%s.command is required", Key)
	}
	if c.Command, err = filepath.Abs(c.Command); err != nil {
		return c, err
	}
	if c.SHA256 != "" {
		if b, err := hex.DecodeString(c.SHA256); err != nil || len(b) != sha256.Size {
			return c, fmt.Errorf("invalid %s.sha256", Key)
		}
	}
	return c, nil
}

// checkBinary checks that the binary exists, is executable and, if given, its sha256.
func (c Config) checkBinary() error {
	fi, err := os.Stat(c.Command)
	if err != nil {
		return fmt.Errorf("plugin binary: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("plugin binary %s is not executable", c.Command)
	}
	if c.SHA256 == "" {
		return nil
	}
	f, err := os.Open(c.Command)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, c.SHA256) {
		return fmt.Errorf("sha256 of binary %s is %s, expected %s", c.Command, got, c.SHA256)
	}
	return nil
}

// Process is an external plugin: it implements plugin.Deployer and the optional interfaces
// (DryRunner, ReleaseInspector, Rollbacker) by forwarding calls to the process, which is
// started on first use and restarted if it exits. Optional interfaces the plugin does not
// implement return an error: actual rollback support is checked with RollbackSupported, from
// the capabilities the plugin declares at startup.
type Process struct {
	name       string
	cfg        Config
	configJSON []byte
	logger     *slog.Logger
	hlog       hclog.Logger

	mu     sync.Mutex
	client *goplugin.Client
	rpc    *external.Client
	closed bool
}

// Name returns the name the plugin is configured with in manent.
func (p *Process) Name() string { return p.name }

// Supports asks the plugin; if the process is unavailable the type is unsupported.
func (p *Process) Supports(artifactType string) bool {
	c, err := p.conn()
	if err != nil {
		p.logger.Error("external plugin unavailable", "error", err)
		return false
	}
	return c.Supports(artifactType)
}

// Validate validates the request in the plugin process.
func (p *Process) Validate(req *plugin.Request) error {
	c, err := p.conn()
	if err != nil {
		return err
	}
	return c.Validate(req)
}

// Deploy runs the deploy in the plugin process.
func (p *Process) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.Deploy(ctx, req)
}

// DryRun runs the dry-run in the plugin process; it returns plugin.ErrDryRunUnsupported if the
// plugin does not implement it.
func (p *Process) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.DryRun(ctx, req)
}

// CurrentRelease reads the release in service through the plugin process.
func (p *Process) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.CurrentRelease(ctx, req)
}

// Rollback runs the rollback in the plugin process.
func (p *Process) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.Rollback(ctx, req, previous)
}

// RollbackSupported starts the process, if needed, and checks that the plugin declared
// rollback support.
func (p *Process) RollbackSupported(context.Context) (bool, error) {
	c, err := p.conn()
	if err != nil {
		return false, err
	}
	return c.Has(external.CapabilityRollback), nil
}

// TrafficSupported implements plugin.TrafficChecker: it starts the process, if needed, and
// reads the capabilities declared for percentage rollouts.
func (p *Process) TrafficSupported(ctx context.Context) (plugin.TrafficSupport, error) {
	c, err := p.conn()
	if err != nil {
		return plugin.TrafficSupport{}, err
	}
	return c.TrafficSupported(ctx)
}

// PrepareSlots forwards plugin.SlotDeployer.PrepareSlots to the process.
func (p *Process) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.PrepareSlots(ctx, req)
}

// DeployCandidate forwards plugin.SlotDeployer.DeployCandidate to the process.
func (p *Process) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.DeployCandidate(ctx, req)
}

// PromoteCandidate forwards plugin.SlotDeployer.PromoteCandidate to the process.
func (p *Process) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	return p.with(func(c *external.Client) error { return c.PromoteCandidate(ctx, req) })
}

// RevertCandidate forwards plugin.SlotDeployer.RevertCandidate to the process.
func (p *Process) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	return p.with(func(c *external.Client) error { return c.RevertCandidate(ctx, req) })
}

// RetirePrevious forwards plugin.SlotDeployer.RetirePrevious to the process.
func (p *Process) RetirePrevious(ctx context.Context, req *plugin.Request) error {
	return p.with(func(c *external.Client) error { return c.RetirePrevious(ctx, req) })
}

// SetCandidateWeight forwards plugin.TrafficRouter.SetCandidateWeight to the process.
func (p *Process) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	return p.with(func(c *external.Client) error { return c.SetCandidateWeight(ctx, req, percent) })
}

// CandidateWeight forwards plugin.TrafficRouter.CandidateWeight to the process.
func (p *Process) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	c, err := p.conn()
	if err != nil {
		return 0, err
	}
	return c.CandidateWeight(ctx, req)
}

// RouteTesters forwards plugin.TesterRouter.RouteTesters to the process.
func (p *Process) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.RouteTesters(ctx, req)
}

// ScaleCandidate forwards plugin.SlotScaler.ScaleCandidate to the process.
func (p *Process) ScaleCandidate(ctx context.Context, req *plugin.Request, percent int) error {
	return p.with(func(c *external.Client) error { return c.ScaleCandidate(ctx, req, percent) })
}

func (p *Process) with(fn func(*external.Client) error) error {
	c, err := p.conn()
	if err != nil {
		return err
	}
	return fn(c)
}

// Close stops the plugin process (go-plugin waits for it to close and then kills it).
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.client != nil {
		p.client.Kill()
		p.client = nil
	}
	return nil
}

// conn returns the client to the process, starting or restarting it if needed.
func (p *Process) conn() (*external.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("external plugin stopped")
	}
	if p.client != nil && !p.client.Exited() && p.alive() {
		return p.rpc, nil
	}
	if p.client != nil {
		p.logger.Warn("plugin process ended, restarting")
		p.client.Kill()
		p.client = nil
	}
	return p.start()
}

// alive checks the connection: right after a crash the process may not look exited yet
// (Exited), but the connection is already closed.
func (p *Process) alive() bool {
	proto, err := p.client.Client()
	return err == nil && proto.Ping() == nil
}

// start starts the process, runs the handshake and sends the configuration.
func (p *Process) start() (*external.Client, error) {
	cmd := exec.Command(p.cfg.Command, p.cfg.Args...)
	cmd.Env = p.env()
	var secure *goplugin.SecureConfig
	if p.cfg.SHA256 != "" {
		sum, _ := hex.DecodeString(p.cfg.SHA256)
		secure = &goplugin.SecureConfig{Checksum: sum, Hash: sha256.New()}
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  external.Handshake,
		Plugins:          external.PluginSet(nil),
		Cmd:              cmd,
		SecureConfig:     secure,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		SkipHostEnv:      true,
		StartTimeout:     p.cfg.StartTimeout,
		Logger:           p.hlog,
	})
	rpc, err := p.handshake(client)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("starting external plugin %q: %w", p.name, err)
	}
	p.client, p.rpc = client, rpc
	return rpc, nil
}

func (p *Process) handshake(client *goplugin.Client) (*external.Client, error) {
	proto, err := client.Client()
	if err != nil {
		return nil, err
	}
	raw, err := proto.Dispense(external.PluginName)
	if err != nil {
		return nil, err
	}
	rpc, ok := raw.(*external.Client)
	if !ok {
		return nil, fmt.Errorf("unexpected client type %T", raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	remote, err := rpc.Configure(ctx, p.configJSON)
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	p.logger.Info("external plugin started", "command", p.cfg.Command, "remote_name", remote, "protocol", client.NegotiatedVersion())
	return rpc, nil
}

// env builds the process environment: only system variables and the configured ones.
func (p *Process) env() []string {
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range p.cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// newHCLogger creates the hclog logger go-plugin requires, which forwards to slog both
// go-plugin's logs and those the plugin writes to stderr.
func newHCLogger(name string, log *slog.Logger) hclog.Logger {
	l := hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: name, Level: hclog.Trace, Output: io.Discard})
	l.RegisterSink(slogSink{log: log.With("component", "plugin-process")})
	return l
}

// slogSink converts hclog messages into slog records.
type slogSink struct {
	log *slog.Logger
}

func (s slogSink) Accept(_ string, level hclog.Level, msg string, args ...any) {
	lvl := slog.LevelDebug
	switch level {
	case hclog.Info:
		lvl = slog.LevelInfo
	case hclog.Warn:
		lvl = slog.LevelWarn
	case hclog.Error:
		lvl = slog.LevelError
	}
	s.log.Log(context.Background(), lvl, msg, args...)
}
