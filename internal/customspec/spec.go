// Package customspec defines the spec of custom steps (pipeline.steps with type), its
// validation, the canonical form captured in the plan and the signature the worker verifies.
// It depends on no other manent package: config, pipeline, engine and customstep use it.
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

// Custom step types.
const (
	TypeCommand = "command"
	TypeHTTP    = "http"
)

// OnTargets runs the command on the targets of the deploy (or of the batch) over SSH.
const OnTargets = "targets"

// Spec limits.
const (
	DefaultTimeout      = 10 * time.Minute
	MaxTimeout          = 6 * time.Hour
	DefaultPollInterval = 10 * time.Second
	MaxOutputs          = 64
	maxParams           = 32
	maxSecrets          = 16
	maxCommandArgs      = 64
)

// Spec is the definition of a custom step, written only in manent.yml.
type Spec struct {
	// Type is command or http; empty for standard steps.
	Type string `yaml:"type" json:"type,omitempty"`
	// Timeout is the maximum duration of a run (default 10m).
	Timeout time.Duration `yaml:"timeout" json:"timeout,omitempty"`
	// Secrets are the logical names of the secrets passed to run (secrets section of the
	// effective descriptor).
	Secrets []string `yaml:"secrets" json:"secrets,omitempty"`
	// Params declares the accepted parameters, with the pattern of their values.
	Params map[string]Param `yaml:"params" json:"params,omitempty"`
	// Outputs declares the only accepted output keys, with the pattern of their values.
	Outputs map[string]Output `yaml:"outputs" json:"outputs,omitempty"`
	// SSH holds the credentials for commands on the targets.
	SSH *SSH `yaml:"ssh" json:"ssh,omitempty"`
	// Run is the real run.
	Run *Action `yaml:"run" json:"run,omitempty"`
	// DryRun, if set, runs in dry-runs; without it the step is skipped.
	DryRun *Action `yaml:"dry_run" json:"dry_run,omitempty"`
}

// Param is a step parameter: its value comes from the environment configuration or, if
// allowed by descriptor_policy.step_params, from the descriptor.
type Param struct {
	// Pattern is a regular expression the whole value must match.
	Pattern  string `yaml:"pattern" json:"pattern"`
	Required bool   `yaml:"required" json:"required,omitempty"`
	Default  string `yaml:"default" json:"default,omitempty"`
}

// Output is an output key of the step.
type Output struct {
	// Pattern is a regular expression the whole value must match.
	Pattern string `yaml:"pattern" json:"pattern"`
	// Path, http only, is the path in the JSON response ($.a.b[0]).
	Path string `yaml:"path" json:"path,omitempty"`
}

// SSH holds user and credentials (logical secret names) for commands on the targets.
type SSH struct {
	// User replaces the target user: better a dedicated user with only the needed privileges.
	User           string `yaml:"user" json:"user,omitempty"`
	KeySecret      string `yaml:"key_secret" json:"key_secret,omitempty"`
	PasswordSecret string `yaml:"password_secret" json:"password_secret,omitempty"`
}

// Action is a run: a command (type command) or an HTTP request (type http).
type Action struct {
	// On is where the command runs: only "targets" in this version.
	On string `yaml:"on" json:"on,omitempty"`
	// RunOnce runs the command on a single target (the first) instead of all.
	RunOnce bool `yaml:"run_once" json:"run_once,omitempty"`
	// Command is the argument list; the first is an absolute path without placeholders.
	Command []string `yaml:"command" json:"command,omitempty"`
	// SHA256 of the Command[0] binary, verified on the target before every run.
	SHA256 string `yaml:"sha256" json:"sha256,omitempty"`

	Method  string            `yaml:"method" json:"method,omitempty"`
	URL     string            `yaml:"url" json:"url,omitempty"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
	Auth    *Auth             `yaml:"auth" json:"auth,omitempty"`
	// JSON is the request body: a structure serialized by manent, with placeholders only in
	// string values.
	JSON any `yaml:"json" json:"json,omitempty"`
	// Expect are the accepted statuses (default: 2xx).
	Expect []int `yaml:"expect" json:"expect,omitempty"`
	Poll   *Poll `yaml:"poll" json:"poll,omitempty"`

	// Secrets, dry_run only, are the dry-run secrets: run's are not passed.
	Secrets []string `yaml:"secrets" json:"secrets,omitempty"`
}

// Auth is HTTP authentication with a secret.
type Auth struct {
	Type     string `yaml:"type" json:"type"` // bearer | basic | header
	Secret   string `yaml:"secret" json:"secret"`
	Username string `yaml:"username" json:"username,omitempty"`
	Header   string `yaml:"header" json:"header,omitempty"`
}

// Poll waits for an asynchronous operation to end by polling a URL.
type Poll struct {
	// URL may use {{ .Response.<field> }} of the first response.
	URL      string        `yaml:"url" json:"url"`
	Interval time.Duration `yaml:"interval" json:"interval,omitempty"`
	Timeout  time.Duration `yaml:"timeout" json:"timeout,omitempty"`
	Until    Condition     `yaml:"until" json:"until"`
	Fail     *Condition    `yaml:"fail" json:"fail,omitempty"`
}

// Condition is true when the value at Path is one of In.
type Condition struct {
	Path string   `yaml:"path" json:"path"`
	In   []string `yaml:"in" json:"in"`
}

// IsCustom reports whether the step is a custom one.
func (s Spec) IsCustom() bool { return s.Type != "" }

// IsZero reports that no spec field is set (standard step).
func (s Spec) IsZero() bool {
	return s.Type == "" && s.Timeout == 0 && len(s.Secrets) == 0 && len(s.Params) == 0 && len(s.Outputs) == 0 &&
		s.SSH == nil && s.Run == nil && s.DryRun == nil
}

// ApplyDefaults fills in the defaults, so the form captured in the plan is explicit.
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
		// The dry-run runs where run runs.
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

// Headers the spec cannot set: manent manages them.
var reservedHeaders = []string{"host", "content-length", "content-type", "transfer-encoding", "connection"}

// Validate checks the spec of step name. allowHTTP allows http URLs.
func (s Spec) Validate(name string, allowHTTP bool) error {
	if !s.IsCustom() {
		if !s.IsZero() {
			return fmt.Errorf("step %q: custom step fields require type", name)
		}
		return nil
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !stepNameRe.MatchString(name) {
		add("invalid name: lowercase letters, digits and '-' (max 63, starting with a letter)")
	}
	if s.Type != TypeCommand && s.Type != TypeHTTP {
		add("invalid type %q (command|http)", s.Type)
	}
	if s.Timeout < time.Second || s.Timeout > MaxTimeout {
		add("invalid timeout %s: between 1s and %s", s.Timeout, MaxTimeout)
	}
	errs = append(errs, checkSecretNames("secrets", s.Secrets))
	if len(s.Params) > maxParams {
		add("params: at most %d", maxParams)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Params)) {
		p := s.Params[k]
		if !keyRe.MatchString(k) {
			add("params.%s: invalid name (lowercase letters, digits and '_', max 31)", k)
		}
		re, err := Compile(p.Pattern)
		switch {
		case p.Pattern == "":
			add("params.%s.pattern is required", k)
		case err != nil:
			add("params.%s.pattern: %v", k, err)
		case p.Default != "" && !re.MatchString(p.Default):
			add("params.%s.default does not match the pattern", k)
		}
	}
	if len(s.Outputs) > MaxOutputs {
		add("outputs: at most %d", MaxOutputs)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Outputs)) {
		o := s.Outputs[k]
		if !keyRe.MatchString(k) {
			add("outputs.%s: invalid name (lowercase letters, digits and '_', max 31)", k)
		}
		if o.Pattern == "" {
			add("outputs.%s.pattern is required", k)
		} else if _, err := Compile(o.Pattern); err != nil {
			add("outputs.%s.pattern: %v", k, err)
		}
		switch {
		case s.Type == TypeHTTP && o.Path == "":
			add("outputs.%s.path is required for http steps", k)
		case s.Type == TypeHTTP:
			if _, err := ParsePath(o.Path); err != nil {
				add("outputs.%s.path: %v", k, err)
			}
		case o.Path != "":
			add("outputs.%s.path is used only with http steps", k)
		}
	}
	if s.Run == nil {
		add("run is required")
	}
	tc := templateCheck{params: s.Params}
	switch s.Type {
	case TypeCommand:
		if s.SSH == nil || (s.SSH.KeySecret == "" && s.SSH.PasswordSecret == "") {
			add("ssh.key_secret or ssh.password_secret is required for command steps")
		} else {
			errs = append(errs, checkSecretNames("ssh", nonEmpty(s.SSH.KeySecret, s.SSH.PasswordSecret)))
		}
		if s.Run != nil {
			errs = append(errs, prefixed("run", s.Run.validateCommand(tc, false)))
			if len(s.Outputs) > 0 && !s.Run.RunOnce {
				add("outputs requires run.run_once: with several targets the outputs would be ambiguous")
			}
		}
		if s.DryRun != nil {
			errs = append(errs, prefixed("dry_run", s.DryRun.validateCommand(tc, true)))
		}
	case TypeHTTP:
		if s.SSH != nil {
			add("ssh is used only with command steps")
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
		add("invalid on %q: in this version only targets", a.On)
	}
	if a.Method != "" || a.URL != "" || len(a.Headers) > 0 || a.Auth != nil || a.JSON != nil || len(a.Expect) > 0 || a.Poll != nil {
		add("http fields are not used with command steps")
	}
	switch {
	case len(a.Command) == 0:
		add("command is required")
	case len(a.Command) > maxCommandArgs:
		add("command: at most %d arguments", maxCommandArgs)
	case !strings.HasPrefix(a.Command[0], "/") || strings.Contains(a.Command[0], "{{"):
		add("command[0] must be an absolute path, without placeholders")
	}
	for i, arg := range a.Command {
		if strings.ContainsAny(arg, "\x00\r\n") {
			add("command[%d]: control characters not allowed", i)
		}
		errs = append(errs, tc.check(fmt.Sprintf("command[%d]", i), arg, false))
	}
	if a.SHA256 != "" && !sha256Re.MatchString(a.SHA256) {
		add("invalid sha256: 64 lowercase hex digits")
	}
	if !dry && len(a.Secrets) > 0 {
		add("secrets is set at step level; inside dry_run it lists the secrets of the dry-run")
	}
	errs = append(errs, checkSecretNames("secrets", a.Secrets))
	return errors.Join(errs...)
}

func (a *Action) validateHTTP(tc templateCheck, allowHTTP, dry bool, timeout time.Duration) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a.On != "" || a.RunOnce || len(a.Command) > 0 || a.SHA256 != "" {
		add("command fields are not used with http steps")
	}
	switch a.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		add("invalid method %q", a.Method)
	}
	if _, err := LiteralOrigin(a.URL, allowHTTP); err != nil {
		add("url: %v", err)
	}
	errs = append(errs, tc.check("url", a.URL, false))
	for _, h := range slices.Sorted(maps.Keys(a.Headers)) {
		if !headerRe.MatchString(h) || slices.Contains(reservedHeaders, strings.ToLower(h)) {
			add("headers.%s: name not allowed", h)
		}
		if a.Auth != nil && (strings.EqualFold(h, "authorization") || strings.EqualFold(h, a.Auth.Header)) {
			add("headers.%s: already set by auth", h)
		}
		errs = append(errs, tc.check("headers."+h, a.Headers[h], false))
	}
	if au := a.Auth; au != nil {
		switch au.Type {
		case "bearer":
		case "basic":
			if au.Username == "" {
				add("auth.username is required with basic")
			}
		case "header":
			if !headerRe.MatchString(au.Header) || slices.Contains(reservedHeaders, strings.ToLower(au.Header)) {
				add("invalid auth.header")
			}
		default:
			add("invalid auth.type %q (bearer|basic|header)", au.Type)
		}
		errs = append(errs, checkSecretNames("auth.secret", []string{au.Secret}))
	}
	if a.JSON != nil {
		if a.Method == "GET" || a.Method == "DELETE" {
			add("json not allowed with %s", a.Method)
		}
		errs = append(errs, walkStrings(a.JSON, "json", func(path, v string) error { return tc.check(path, v, false) }))
		if _, err := json.Marshal(a.JSON); err != nil {
			add("json cannot be serialized: %v", err)
		}
	}
	for _, c := range a.Expect {
		if c < 100 || c > 599 {
			add("expect: invalid status %d", c)
		}
	}
	if p := a.Poll; p != nil {
		if _, err := LiteralOrigin(p.URL, allowHTTP); err != nil {
			add("poll.url: %v", err)
		}
		errs = append(errs, tc.check("poll.url", p.URL, true))
		if p.Interval < time.Second {
			add("poll.interval: at least 1s")
		}
		if p.Timeout < p.Interval || p.Timeout > timeout {
			add("poll.timeout: between poll.interval and the step timeout (%s)", timeout)
		}
		for name, c := range map[string]*Condition{"until": &p.Until, "fail": p.Fail} {
			if c == nil {
				continue
			}
			if _, err := ParsePath(c.Path); err != nil {
				add("poll.%s.path: %v", name, err)
			}
			if len(c.In) == 0 {
				add("poll.%s.in is required", name)
			}
		}
	}
	if !dry && len(a.Secrets) > 0 {
		add("secrets is set at step level; inside dry_run it lists the secrets of the dry-run")
	}
	errs = append(errs, checkSecretNames("secrets", a.Secrets))
	if dry && a.Auth != nil && !slices.Contains(a.Secrets, a.Auth.Secret) {
		add("auth.secret must be among the secrets of the dry-run (dry_run.secrets)")
	}
	return errors.Join(errs...)
}

// authSecretDeclared checks that run's HTTP authentication uses a declared secret.
func (s Spec) authSecretDeclared() error {
	if s.Type == TypeHTTP && s.Run != nil && s.Run.Auth != nil && !slices.Contains(s.Secrets, s.Run.Auth.Secret) {
		return errors.New("run.auth.secret must be among the step secrets (secrets)")
	}
	return nil
}

// LiteralOrigin returns scheme and host of the URL, which must be written without
// placeholders: substituted values can change only path and query, never the destination.
func LiteralOrigin(raw string, allowHTTP bool) (string, error) {
	lit := raw
	if i := strings.Index(raw, "{{"); i >= 0 {
		lit = raw[:i]
	}
	scheme, rest, ok := strings.Cut(lit, "://")
	if !ok {
		return "", errors.New("scheme and host must be written in full, without placeholders")
	}
	host, _, hasPath := strings.Cut(rest, "/")
	if host == "" {
		return "", errors.New("scheme and host must be written in full, without placeholders")
	}
	if !hasPath && len(lit) < len(raw) {
		return "", errors.New("the host must be followed by '/' before any placeholder")
	}
	u, err := url.Parse(scheme + "://" + host + "/")
	switch {
	case err != nil:
		return "", err
	case u.Host == "" || u.User != nil:
		return "", errors.New("host missing or with credentials")
	case u.Scheme != "https" && !(allowHTTP && u.Scheme == "http"):
		return "", fmt.Errorf("scheme %q not allowed (https, or http with pipeline.custom.allow_http)", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Compile compiles an anchored pattern: the whole value must match it.
func Compile(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + pattern + `)$`)
}

func checkSecretNames(field string, names []string) error {
	var errs []error
	if len(names) > maxSecrets {
		errs = append(errs, fmt.Errorf("%s: at most %d", field, maxSecrets))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !secretNameRe.MatchString(n) || seen[n] {
			errs = append(errs, fmt.Errorf("%s: invalid or duplicate secret name %q", field, n))
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

// walkStrings visits the string values of a JSON structure.
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

// Roots allowed in placeholders.
var templateRoots = []string{"Application", "Environment", "Version", "Artifact", "DeploymentID", "SHA256", "DryRun", "Params", "Outputs"}

// templateCheck checks the placeholders: syntax, known roots and declared parameters.
type templateCheck struct {
	params map[string]Param
}

// NewTemplate creates a template with manent's rules: a missing key is an error, never
// "<no value>".
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
			errs = append(errs, fmt.Errorf("%s: unknown placeholder .%s", field, root))
		case root == "Params" && len(idents) > 1:
			if _, ok := tc.params[idents[1]]; !ok {
				errs = append(errs, fmt.Errorf("%s: parameter %q not declared in params", field, idents[1]))
			}
		}
	})
	return errors.Join(errs...)
}

// visitFields calls fn for every field reference (.A.B) of the template.
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
