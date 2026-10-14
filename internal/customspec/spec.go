// Package customspec definisce la specifica degli step personalizzati (pipeline.steps con type),
// la sua validazione, la forma canonica fotografata nel piano e la firma che il worker verifica.
// Non dipende da altri package di manent: lo usano config, pipeline, engine e customstep.
package customspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
)

// Tipi di step personalizzato.
const (
	TypeCommand = "command"
	TypeHTTP    = "http"
)

// OnTargets esegue il comando sui target del deploy (o del lotto) via SSH.
const OnTargets = "targets"

// Limiti della specifica.
const (
	DefaultTimeout      = 10 * time.Minute
	MaxTimeout          = 6 * time.Hour
	DefaultPollInterval = 10 * time.Second
	MaxOutputs          = 64
	maxParams           = 32
	maxSecrets          = 16
	maxCommandArgs      = 64
)

// Spec è la definizione di uno step personalizzato, scritta solo in manent.yml.
type Spec struct {
	// Type è command o http; vuoto per gli step standard.
	Type string `yaml:"type" json:"type,omitempty"`
	// Timeout è la durata massima di un'esecuzione (default 10m).
	Timeout time.Duration `yaml:"timeout" json:"timeout,omitempty"`
	// Secrets sono i nomi logici dei segreti passati a run (sezione secrets del descrittore effettivo).
	Secrets []string `yaml:"secrets" json:"secrets,omitempty"`
	// Params dichiara i parametri accettati, con il pattern dei valori.
	Params map[string]Param `yaml:"params" json:"params,omitempty"`
	// Outputs dichiara le sole chiavi di output accettate, con il pattern dei valori.
	Outputs map[string]Output `yaml:"outputs" json:"outputs,omitempty"`
	// SSH sono le credenziali per i comandi sui target.
	SSH *SSH `yaml:"ssh" json:"ssh,omitempty"`
	// Run è l'esecuzione reale.
	Run *Action `yaml:"run" json:"run,omitempty"`
	// DryRun, se presente, è eseguito nei dry-run; senza, lo step viene saltato.
	DryRun *Action `yaml:"dry_run" json:"dry_run,omitempty"`
}

// Param è un parametro dello step: il valore arriva dalla configurazione di ambiente o, se
// ammesso da descriptor_policy.step_params, dal descrittore.
type Param struct {
	// Pattern è un'espressione regolare che il valore deve rispettare per intero.
	Pattern  string `yaml:"pattern" json:"pattern"`
	Required bool   `yaml:"required" json:"required,omitempty"`
	Default  string `yaml:"default" json:"default,omitempty"`
}

// Output è una chiave di output dello step.
type Output struct {
	// Pattern è un'espressione regolare che il valore deve rispettare per intero.
	Pattern string `yaml:"pattern" json:"pattern"`
	// Path, solo per http, è il percorso nella risposta JSON ($.a.b[0]).
	Path string `yaml:"path" json:"path,omitempty"`
}

// SSH sono utente e credenziali (nomi logici di segreti) per i comandi sui target.
type SSH struct {
	// User sostituisce l'utente del target: meglio un utente dedicato, con i soli privilegi necessari.
	User           string `yaml:"user" json:"user,omitempty"`
	KeySecret      string `yaml:"key_secret" json:"key_secret,omitempty"`
	PasswordSecret string `yaml:"password_secret" json:"password_secret,omitempty"`
}

// Action è un'esecuzione: un comando (type command) o una richiesta HTTP (type http).
type Action struct {
	// On è dove gira il comando: solo "targets" in questa versione.
	On string `yaml:"on" json:"on,omitempty"`
	// RunOnce esegue il comando su un solo target (il primo) invece che su tutti.
	RunOnce bool `yaml:"run_once" json:"run_once,omitempty"`
	// Command è la lista degli argomenti; il primo è un percorso assoluto senza segnaposto.
	Command []string `yaml:"command" json:"command,omitempty"`
	// SHA256 del binario Command[0], verificato sul target prima di ogni esecuzione.
	SHA256 string `yaml:"sha256" json:"sha256,omitempty"`

	Method  string            `yaml:"method" json:"method,omitempty"`
	URL     string            `yaml:"url" json:"url,omitempty"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
	Auth    *Auth             `yaml:"auth" json:"auth,omitempty"`
	// JSON è il corpo della richiesta: una struttura serializzata da manent, con segnaposto
	// solo nei valori stringa.
	JSON any `yaml:"json" json:"json,omitempty"`
	// Expect sono gli status accettati (default: 2xx).
	Expect []int `yaml:"expect" json:"expect,omitempty"`
	Poll   *Poll `yaml:"poll" json:"poll,omitempty"`

	// Secrets, solo in dry_run, sono i segreti del dry-run: quelli di run non vengono passati.
	Secrets []string `yaml:"secrets" json:"secrets,omitempty"`
}

// Auth è l'autenticazione HTTP con un segreto.
type Auth struct {
	Type     string `yaml:"type" json:"type"` // bearer | basic | header
	Secret   string `yaml:"secret" json:"secret"`
	Username string `yaml:"username" json:"username,omitempty"`
	Header   string `yaml:"header" json:"header,omitempty"`
}

// Poll attende che un'operazione asincrona si concluda interrogando un URL.
type Poll struct {
	// URL può usare {{ .Response.<campo> }} della prima risposta.
	URL      string        `yaml:"url" json:"url"`
	Interval time.Duration `yaml:"interval" json:"interval,omitempty"`
	Timeout  time.Duration `yaml:"timeout" json:"timeout,omitempty"`
	Until    Condition     `yaml:"until" json:"until"`
	Fail     *Condition    `yaml:"fail" json:"fail,omitempty"`
}

// Condition è vera quando il valore in Path è uno di In.
type Condition struct {
	Path string   `yaml:"path" json:"path"`
	In   []string `yaml:"in" json:"in"`
}

// IsCustom indica se lo step è personalizzato.
func (s Spec) IsCustom() bool { return s.Type != "" }

// IsZero indica che nessun campo della specifica è valorizzato (step standard).
func (s Spec) IsZero() bool {
	return s.Type == "" && s.Timeout == 0 && len(s.Secrets) == 0 && len(s.Params) == 0 && len(s.Outputs) == 0 &&
		s.SSH == nil && s.Run == nil && s.DryRun == nil
}

// ApplyDefaults completa i valori di default, così la forma fotografata nel piano è esplicita.
func (s *Spec) ApplyDefaults() {
	if !s.IsCustom() {
		return
	}
	if s.Timeout == 0 {
		s.Timeout = DefaultTimeout
	}
	for _, a := range []*Action{s.Run, s.DryRun} {
		if a == nil {
			continue
		}
		if s.Type == TypeCommand && a.On == "" {
			a.On = OnTargets
		}
		if s.Type == TypeHTTP {
			a.Method = strings.ToUpper(a.Method)
			if a.Method == "" {
				a.Method = "POST"
			}
			if p := a.Poll; p != nil {
				if p.Interval == 0 {
					p.Interval = DefaultPollInterval
				}
				if p.Timeout == 0 {
					p.Timeout = s.Timeout
				}
			}
		}
	}
	if s.Type == TypeCommand && s.DryRun != nil && s.Run != nil {
		// Il dry-run gira dove gira run.
		s.DryRun.On, s.DryRun.RunOnce = s.Run.On, s.Run.RunOnce
	}
}

var (
	stepNameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	keyRe        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sha256Re     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	headerRe     = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// Header che la specifica non può impostare: li gestisce manent.
var reservedHeaders = []string{"host", "content-length", "content-type", "transfer-encoding", "connection"}

// Validate controlla la specifica dello step name. allowHTTP ammette URL http.
func (s Spec) Validate(name string, allowHTTP bool) error {
	if !s.IsCustom() {
		if !s.IsZero() {
			return fmt.Errorf("step %q: i campi degli step personalizzati richiedono type", name)
		}
		return nil
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !stepNameRe.MatchString(name) {
		add("nome non valido: minuscole, cifre e '-' (max 63, iniziale una lettera)")
	}
	if s.Type != TypeCommand && s.Type != TypeHTTP {
		add("type %q non valido (command|http)", s.Type)
	}
	if s.Timeout < time.Second || s.Timeout > MaxTimeout {
		add("timeout %s non valido: tra 1s e %s", s.Timeout, MaxTimeout)
	}
	errs = append(errs, checkSecretNames("secrets", s.Secrets))
	if len(s.Params) > maxParams {
		add("params: al massimo %d", maxParams)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Params)) {
		p := s.Params[k]
		if !keyRe.MatchString(k) {
			add("params.%s: nome non valido (minuscole, cifre e '_', max 31)", k)
		}
		re, err := Compile(p.Pattern)
		switch {
		case p.Pattern == "":
			add("params.%s.pattern obbligatorio", k)
		case err != nil:
			add("params.%s.pattern: %v", k, err)
		case p.Default != "" && !re.MatchString(p.Default):
			add("params.%s.default non rispetta il pattern", k)
		}
	}
	if len(s.Outputs) > MaxOutputs {
		add("outputs: al massimo %d", MaxOutputs)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Outputs)) {
		o := s.Outputs[k]
		if !keyRe.MatchString(k) {
			add("outputs.%s: nome non valido (minuscole, cifre e '_', max 31)", k)
		}
		if o.Pattern == "" {
			add("outputs.%s.pattern obbligatorio", k)
		} else if _, err := Compile(o.Pattern); err != nil {
			add("outputs.%s.pattern: %v", k, err)
		}
		switch {
		case s.Type == TypeHTTP && o.Path == "":
			add("outputs.%s.path obbligatorio per gli step http", k)
		case s.Type == TypeHTTP:
			if _, err := ParsePath(o.Path); err != nil {
				add("outputs.%s.path: %v", k, err)
			}
		case o.Path != "":
			add("outputs.%s.path si usa solo con gli step http", k)
		}
	}
	if s.Run == nil {
		add("run obbligatorio")
	}
	tc := templateCheck{params: s.Params}
	switch s.Type {
	case TypeCommand:
		if s.SSH == nil || (s.SSH.KeySecret == "" && s.SSH.PasswordSecret == "") {
			add("ssh.key_secret o ssh.password_secret obbligatorio per gli step command")
		} else {
			errs = append(errs, checkSecretNames("ssh", nonEmpty(s.SSH.KeySecret, s.SSH.PasswordSecret)))
		}
		if s.Run != nil {
			errs = append(errs, prefixed("run", s.Run.validateCommand(tc, false)))
			if len(s.Outputs) > 0 && !s.Run.RunOnce {
				add("outputs richiede run.run_once: con più target gli output sarebbero ambigui")
			}
		}
		if s.DryRun != nil {
			errs = append(errs, prefixed("dry_run", s.DryRun.validateCommand(tc, true)))
		}
	case TypeHTTP:
		if s.SSH != nil {
			add("ssh si usa solo con gli step command")
		}
		if s.Run != nil {
			errs = append(errs, prefixed("run", s.Run.validateHTTP(tc, allowHTTP, false, s.Timeout)))
		}
		if s.DryRun != nil {
			errs = append(errs, prefixed("dry_run", s.DryRun.validateHTTP(tc, allowHTTP, true, s.Timeout)))
		}
		errs = append(errs, s.authSecretDeclared())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("step %q: %w", name, err)
	}
	return nil
}

func (a *Action) validateCommand(tc templateCheck, dry bool) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a.On != OnTargets {
		add("on %q non valido: in questa versione solo targets", a.On)
	}
	if a.Method != "" || a.URL != "" || len(a.Headers) > 0 || a.Auth != nil || a.JSON != nil || len(a.Expect) > 0 || a.Poll != nil {
		add("i campi http non si usano con gli step command")
	}
	switch {
	case len(a.Command) == 0:
		add("command obbligatorio")
	case len(a.Command) > maxCommandArgs:
		add("command: al massimo %d argomenti", maxCommandArgs)
	case !strings.HasPrefix(a.Command[0], "/") || strings.Contains(a.Command[0], "{{"):
		add("command[0] deve essere un percorso assoluto, senza segnaposto")
	}
	for i, arg := range a.Command {
		if strings.ContainsAny(arg, "\x00\r\n") {
			add("command[%d]: caratteri di controllo non ammessi", i)
		}
		errs = append(errs, tc.check(fmt.Sprintf("command[%d]", i), arg, false))
	}
	if a.SHA256 != "" && !sha256Re.MatchString(a.SHA256) {
		add("sha256 non valido: 64 cifre esadecimali minuscole")
	}
	if !dry && len(a.Secrets) > 0 {
		add("secrets si indica a livello di step; dentro dry_run elenca i segreti del dry-run")
	}
	errs = append(errs, checkSecretNames("secrets", a.Secrets))
	return errors.Join(errs...)
}

func (a *Action) validateHTTP(tc templateCheck, allowHTTP, dry bool, timeout time.Duration) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a.On != "" || a.RunOnce || len(a.Command) > 0 || a.SHA256 != "" {
		add("i campi dei comandi non si usano con gli step http")
	}
	switch a.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		add("method %q non valido", a.Method)
	}
	if _, err := LiteralOrigin(a.URL, allowHTTP); err != nil {
		add("url: %v", err)
	}
	errs = append(errs, tc.check("url", a.URL, false))
	for _, h := range slices.Sorted(maps.Keys(a.Headers)) {
		if !headerRe.MatchString(h) || slices.Contains(reservedHeaders, strings.ToLower(h)) {
			add("headers.%s: nome non ammesso", h)
		}
		if a.Auth != nil && (strings.EqualFold(h, "authorization") || strings.EqualFold(h, a.Auth.Header)) {
			add("headers.%s: già impostato da auth", h)
		}
		errs = append(errs, tc.check("headers."+h, a.Headers[h], false))
	}
	if au := a.Auth; au != nil {
		switch au.Type {
		case "bearer":
		case "basic":
			if au.Username == "" {
				add("auth.username obbligatorio con basic")
			}
		case "header":
			if !headerRe.MatchString(au.Header) || slices.Contains(reservedHeaders, strings.ToLower(au.Header)) {
				add("auth.header non valido")
			}
		default:
			add("auth.type %q non valido (bearer|basic|header)", au.Type)
		}
		errs = append(errs, checkSecretNames("auth.secret", []string{au.Secret}))
	}
	if a.JSON != nil {
		if a.Method == "GET" || a.Method == "DELETE" {
			add("json non ammesso con %s", a.Method)
		}
		errs = append(errs, walkStrings(a.JSON, "json", func(path, v string) error { return tc.check(path, v, false) }))
		if _, err := json.Marshal(a.JSON); err != nil {
			add("json non serializzabile: %v", err)
		}
	}
	for _, c := range a.Expect {
		if c < 100 || c > 599 {
			add("expect: status %d non valido", c)
		}
	}
	if p := a.Poll; p != nil {
		if _, err := LiteralOrigin(p.URL, allowHTTP); err != nil {
			add("poll.url: %v", err)
		}
		errs = append(errs, tc.check("poll.url", p.URL, true))
		if p.Interval < time.Second {
			add("poll.interval: almeno 1s")
		}
		if p.Timeout < p.Interval || p.Timeout > timeout {
			add("poll.timeout: tra poll.interval e il timeout dello step (%s)", timeout)
		}
		for name, c := range map[string]*Condition{"until": &p.Until, "fail": p.Fail} {
			if c == nil {
				continue
			}
			if _, err := ParsePath(c.Path); err != nil {
				add("poll.%s.path: %v", name, err)
			}
			if len(c.In) == 0 {
				add("poll.%s.in obbligatorio", name)
			}
		}
	}
	if !dry && len(a.Secrets) > 0 {
		add("secrets si indica a livello di step; dentro dry_run elenca i segreti del dry-run")
	}
	errs = append(errs, checkSecretNames("secrets", a.Secrets))
	if dry && a.Auth != nil && !slices.Contains(a.Secrets, a.Auth.Secret) {
		add("auth.secret deve essere tra i segreti del dry-run (dry_run.secrets)")
	}
	return errors.Join(errs...)
}

// authSecretDeclared controlla che l'autenticazione HTTP di run usi un segreto dichiarato.
func (s Spec) authSecretDeclared() error {
	if s.Type == TypeHTTP && s.Run != nil && s.Run.Auth != nil && !slices.Contains(s.Secrets, s.Run.Auth.Secret) {
		return errors.New("run.auth.secret deve essere tra i segreti dello step (secrets)")
	}
	return nil
}

// LiteralOrigin restituisce schema e host dell'URL, che devono essere scritti senza segnaposto:
// i valori sostituiti possono cambiare solo percorso e query, mai la destinazione.
func LiteralOrigin(raw string, allowHTTP bool) (string, error) {
	lit := raw
	if i := strings.Index(raw, "{{"); i >= 0 {
		lit = raw[:i]
	}
	scheme, rest, ok := strings.Cut(lit, "://")
	if !ok {
		return "", errors.New("schema e host vanno scritti per intero, senza segnaposto")
	}
	host, _, hasPath := strings.Cut(rest, "/")
	if host == "" {
		return "", errors.New("schema e host vanno scritti per intero, senza segnaposto")
	}
	if !hasPath && len(lit) < len(raw) {
		return "", errors.New("l'host deve essere seguito da '/' prima di qualunque segnaposto")
	}
	u, err := url.Parse(scheme + "://" + host + "/")
	switch {
	case err != nil:
		return "", err
	case u.Host == "" || u.User != nil:
		return "", errors.New("host mancante o con credenziali")
	case u.Scheme != "https" && !(allowHTTP && u.Scheme == "http"):
		return "", fmt.Errorf("schema %q non ammesso (https, o http con pipeline.custom.allow_http)", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Compile compila un pattern ancorato: il valore deve rispettarlo per intero.
func Compile(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + pattern + `)$`)
}

func checkSecretNames(field string, names []string) error {
	var errs []error
	if len(names) > maxSecrets {
		errs = append(errs, fmt.Errorf("%s: al massimo %d", field, maxSecrets))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !secretNameRe.MatchString(n) || seen[n] {
			errs = append(errs, fmt.Errorf("%s: nome di segreto %q non valido o duplicato", field, n))
		}
		seen[n] = true
	}
	return errors.Join(errs...)
}

func nonEmpty(v ...string) []string {
	return slices.DeleteFunc(v, func(s string) bool { return s == "" })
}

func prefixed(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// walkStrings visita i valori stringa di una struttura JSON.
func walkStrings(v any, path string, fn func(path, s string) error) error {
	var errs []error
	switch t := v.(type) {
	case string:
		errs = append(errs, fn(path, t))
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			errs = append(errs, walkStrings(t[k], path+"."+k, fn))
		}
	case []any:
		for i, e := range t {
			errs = append(errs, walkStrings(e, fmt.Sprintf("%s[%d]", path, i), fn))
		}
	}
	return errors.Join(errs...)
}

// Radici ammesse nei segnaposto.
var templateRoots = []string{"Application", "Environment", "Version", "Artifact", "DeploymentID", "SHA256", "DryRun", "Params", "Outputs"}

// templateCheck verifica i segnaposto: sintassi, radici note e parametri dichiarati.
type templateCheck struct {
	params map[string]Param
}

// NewTemplate crea un template con le regole di manent: una chiave mancante è un errore,
// mai "<no value>".
func NewTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(text)
}

func (tc templateCheck) check(field, text string, response bool) error {
	if !strings.Contains(text, "{{") {
		return nil
	}
	t, err := NewTemplate(field, text)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	var errs []error
	visitFields(t.Tree.Root, func(idents []string) {
		root := idents[0]
		switch {
		case root == "Response" && response:
		case !slices.Contains(templateRoots, root):
			errs = append(errs, fmt.Errorf("%s: segnaposto .%s sconosciuto", field, root))
		case root == "Params" && len(idents) > 1:
			if _, ok := tc.params[idents[1]]; !ok {
				errs = append(errs, fmt.Errorf("%s: parametro %q non dichiarato in params", field, idents[1]))
			}
		}
	})
	return errors.Join(errs...)
}

// visitFields chiama fn per ogni riferimento a un campo (.A.B) del template.
func visitFields(n parse.Node, fn func([]string)) {
	switch t := n.(type) {
	case *parse.ListNode:
		if t == nil {
			return
		}
		for _, c := range t.Nodes {
			visitFields(c, fn)
		}
	case *parse.ActionNode:
		visitFields(t.Pipe, fn)
	case *parse.PipeNode:
		if t == nil {
			return
		}
		for _, c := range t.Cmds {
			visitFields(c, fn)
		}
	case *parse.CommandNode:
		for _, a := range t.Args {
			visitFields(a, fn)
		}
	case *parse.FieldNode:
		fn(t.Ident)
	case *parse.ChainNode:
		visitFields(t.Node, fn)
	case *parse.IfNode:
		visitFields(t.Pipe, fn)
		visitFields(t.List, fn)
		visitFields(t.ElseList, fn)
	case *parse.RangeNode:
		visitFields(t.Pipe, fn)
		visitFields(t.List, fn)
		visitFields(t.ElseList, fn)
	case *parse.WithNode:
		visitFields(t.Pipe, fn)
		visitFields(t.List, fn)
		visitFields(t.ElseList, fn)
	}
}
