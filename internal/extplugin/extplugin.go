// Package extplugin avvia e gestisce i plugin di deploy esterni: binari separati che parlano
// con manent via gRPC (pkg/plugin/external) e che appaiono al resto del motore come un normale
// plugin.Deployer. Si attivano con la chiave "external" in plugins.<nome> della configurazione.
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

// Key è la chiave che, in plugins.<nome>, identifica un plugin esterno.
const Key = "external"

// Config è la sezione external di un plugin.
type Config struct {
	// Command è il percorso del binario del plugin.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// SHA256 del binario: verificato all'attivazione e a ogni avvio del processo.
	SHA256 string `yaml:"sha256"`
	// Env sono le variabili passate al plugin. Le variabili del worker non vengono ereditate,
	// salvo quelle di sistema in inheritedEnv: possono contenere segreti (es. riferimenti env:).
	Env map[string]string `yaml:"env"`
	// StartTimeout è il tempo massimo per l'avvio del processo (default 1m).
	StartTimeout time.Duration `yaml:"start_timeout"`
}

// inheritedEnv sono le sole variabili del worker passate al plugin.
var inheritedEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG"}

// callTimeout limita Configure all'avvio del processo.
const callTimeout = 30 * time.Second

// IsExternal indica se la configurazione di un plugin descrive un plugin esterno.
func IsExternal(cfg map[string]any) bool {
	_, ok := cfg[Key]
	return ok
}

// NewFactory restituisce la factory del plugin esterno registrato con il nome indicato.
// L'attivazione verifica configurazione e binario ma non avvia il processo: viene avviato al
// primo utilizzo, così i comandi CLI (che attivano i plugin solo per calcolare il piano) non
// eseguono mai il binario.
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
			return nil, fmt.Errorf("configurazione del plugin non serializzabile in JSON: %w", err)
		}
		if c.SHA256 == "" {
			logger.Warn("plugin esterno senza sha256: il binario non viene verificato", "command", c.Command)
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
		return c, fmt.Errorf("sezione %s non valida: %w", Key, err)
	}
	if c.Command == "" {
		return c, fmt.Errorf("%s.command obbligatorio", Key)
	}
	if c.Command, err = filepath.Abs(c.Command); err != nil {
		return c, err
	}
	if c.SHA256 != "" {
		if b, err := hex.DecodeString(c.SHA256); err != nil || len(b) != sha256.Size {
			return c, fmt.Errorf("%s.sha256 non valido", Key)
		}
	}
	return c, nil
}

// checkBinary verifica che il binario esista, sia eseguibile e, se indicato, il suo sha256.
func (c Config) checkBinary() error {
	fi, err := os.Stat(c.Command)
	if err != nil {
		return fmt.Errorf("binario del plugin: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("binario del plugin %s non eseguibile", c.Command)
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
		return fmt.Errorf("sha256 del binario %s è %s, atteso %s", c.Command, got, c.SHA256)
	}
	return nil
}

// Process è un plugin esterno: implementa plugin.Deployer e le interfacce opzionali
// (DryRunner, ReleaseInspector, Rollbacker) inoltrando le chiamate al processo, che viene
// avviato al primo uso e riavviato se termina. Le interfacce opzionali che il plugin non
// implementa restituiscono un errore: il supporto effettivo al rollback si verifica con
// RollbackSupported, dalle capacità dichiarate dal plugin all'avvio.
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

// Name restituisce il nome con cui il plugin è configurato in manent.
func (p *Process) Name() string { return p.name }

// Supports interroga il plugin; se il processo non è disponibile il tipo risulta non supportato.
func (p *Process) Supports(artifactType string) bool {
	c, err := p.conn()
	if err != nil {
		p.logger.Error("plugin esterno non disponibile", "error", err)
		return false
	}
	return c.Supports(artifactType)
}

// Validate valida la richiesta nel processo del plugin.
func (p *Process) Validate(req *plugin.Request) error {
	c, err := p.conn()
	if err != nil {
		return err
	}
	return c.Validate(req)
}

// Deploy esegue il deploy nel processo del plugin.
func (p *Process) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.Deploy(ctx, req)
}

// DryRun esegue il dry-run nel processo del plugin; restituisce plugin.ErrDryRunUnsupported
// se il plugin non lo implementa.
func (p *Process) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.DryRun(ctx, req)
}

// CurrentRelease legge la release in esercizio tramite il processo del plugin.
func (p *Process) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.CurrentRelease(ctx, req)
}

// Rollback esegue il rollback nel processo del plugin.
func (p *Process) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	c, err := p.conn()
	if err != nil {
		return nil, err
	}
	return c.Rollback(ctx, req, previous)
}

// RollbackSupported avvia il processo, se necessario, e verifica che il plugin abbia
// dichiarato il supporto al rollback.
func (p *Process) RollbackSupported(context.Context) (bool, error) {
	c, err := p.conn()
	if err != nil {
		return false, err
	}
	return c.Has(external.CapabilityRollback), nil
}

// Close arresta il processo del plugin (go-plugin attende la chiusura e poi lo termina).
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

// conn restituisce il client verso il processo, avviandolo o riavviandolo se necessario.
func (p *Process) conn() (*external.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("plugin esterno arrestato")
	}
	if p.client != nil && !p.client.Exited() && p.alive() {
		return p.rpc, nil
	}
	if p.client != nil {
		p.logger.Warn("processo del plugin terminato, riavvio")
		p.client.Kill()
		p.client = nil
	}
	return p.start()
}

// alive verifica la connessione: subito dopo un crash il processo può non risultare ancora
// terminato (Exited), ma la connessione è già chiusa.
func (p *Process) alive() bool {
	proto, err := p.client.Client()
	return err == nil && proto.Ping() == nil
}

// start avvia il processo, esegue l'handshake e invia la configurazione.
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
		return nil, fmt.Errorf("avvio del plugin esterno %q: %w", p.name, err)
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
		return nil, fmt.Errorf("tipo di client inatteso %T", raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	remote, err := rpc.Configure(ctx, p.configJSON)
	if err != nil {
		return nil, fmt.Errorf("configurazione: %w", err)
	}
	p.logger.Info("plugin esterno avviato", "command", p.cfg.Command, "remote_name", remote, "protocol", client.NegotiatedVersion())
	return rpc, nil
}

// env costruisce l'ambiente del processo: solo variabili di sistema e quelle configurate.
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

// newHCLogger crea il logger hclog richiesto da go-plugin, che inoltra a slog sia i log di
// go-plugin sia quelli scritti dal plugin su stderr.
func newHCLogger(name string, log *slog.Logger) hclog.Logger {
	l := hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: name, Level: hclog.Trace, Output: io.Discard})
	l.RegisterSink(slogSink{log: log.With("component", "plugin-process")})
	return l
}

// slogSink converte i messaggi hclog in record slog.
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
