package customstep

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// SSHAuth holds the credentials resolved at connection time.
type SSHAuth struct {
	PrivateKey string
	Password   string
}

// Dialer opens connections to the targets; abstracted for tests.
type Dialer interface {
	Dial(ctx context.Context, t descriptor.Target, user string, auth SSHAuth) (Conn, error)
}

// Conn runs commands on a target.
type Conn interface {
	// Exec runs cmd with the target's login shell and returns the exit code.
	// The error means a transport problem or a command ended without a code.
	Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Close() error
}

// Exit codes with a meaning for manent.
const (
	// ExitTempFail (EX_TEMPFAIL) asks for a retry.
	ExitTempFail = 75
	// exitTimeout is the code of timeout(1) when time runs out.
	exitTimeout = 124
)

// Limits of the outputs file.
const maxOutputFile = 16 << 10

// script runs the command on the target. It receives the attempt directory and the arguments
// as positional parameters, so no value goes through shell interpretation; it loads the
// MANENT_* variables from the environment file (not from the command line, visible with ps)
// and, if available, limits the duration with timeout(1), so the process does not outlive the
// step. An interruption removes the secrets directory.
const script = `d=$1; shift
trap 'rm -rf -- "$d"; exit 143' HUP INT TERM
set -a; . "$d/env"; set +a
: > "$d/output"
if command -v timeout >/dev/null 2>&1; then timeout -k 10 %d "$@"; else "$@"; fi`

// command runs the command on every target of the deploy (or of the batch), in sequence, or
// on the first one with run_once. It returns the outputs of the last target.
func (r *run) command(ctx context.Context) (map[string]string, error) {
	targets := slices.Clone(r.d.Deploy.Targets)
	if len(targets) == 0 {
		return nil, permanent("no target to run the command on")
	}
	if r.action.RunOnce {
		targets = targets[:1]
	}
	args := make([]string, len(r.action.Command))
	for i, a := range r.action.Command {
		v, err := renderArg(i, a, r.data)
		if err != nil {
			// Spec and values do not change between attempts.
			return nil, permanent("%w", err)
		}
		args[i] = v
	}
	auth, err := r.sshAuth(ctx)
	if err != nil {
		return nil, err
	}
	budget := &logBudget{left: maxLogBytes}
	var out map[string]string
	for _, t := range targets {
		r.targets = append(r.targets, safeHost(t))
		if out, err = r.onTarget(ctx, t, args, auth, budget); err != nil {
			return nil, fmt.Errorf("target %s: %w", safeHost(t), err)
		}
	}
	return out, nil
}

func (r *run) sshAuth(ctx context.Context) (SSHAuth, error) {
	var a SSHAuth
	ssh := r.s.doc.SSH
	res := r.s.b.Secrets
	for name, dst := range map[string]*string{ssh.KeySecret: &a.PrivateKey, ssh.PasswordSecret: &a.Password} {
		if name == "" {
			continue
		}
		ref, _ := r.d.SecretRefByName(name)
		v, err := res.Resolve(ctx, ref)
		if err != nil {
			return a, fmt.Errorf("SSH credentials: %w", err)
		}
		*dst = v
	}
	return a, nil
}

// onTarget runs the protocol on a target: attempt directory, environment and secrets files,
// binary check, command, output reading and cleanup.
func (r *run) onTarget(ctx context.Context, t descriptor.Target, args []string, auth SSHAuth, budget *logBudget) (map[string]string, error) {
	user := t.User
	if r.s.doc.SSH.User != "" {
		user = r.s.doc.SSH.User
	}
	if user == "" {
		return nil, permanent("SSH user not set (target.user or ssh.user)")
	}
	conn, err := r.s.b.Dialer.Dial(ctx, t, user, auth)
	if err != nil {
		return nil, temporary("SSH connection: %w", err)
	}
	defer conn.Close()

	base := path.Join(r.s.b.SecretsDir, pathSafe(r.in.DeploymentID), r.s.ps.Name)
	dir := path.Join(base, nonce())
	// Previous attempts (even those interrupted by a crash) are removed before starting.
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if code, err := conn.Exec(c, "rm -rf -- "+Quote(base), nil, io.Discard, io.Discard); err != nil || code != 0 {
			r.log.WarnContext(ctx, "pulizia della directory dei segreti fallita: verrà ripetuta al prossimo tentativo", "target", t.Host, "code", code, "error", err)
		}
	}()
	if err := r.exec(ctx, conn, "umask 077 && rm -rf -- "+Quote(base)+" && mkdir -p -- "+Quote(path.Join(dir, "secrets")), nil, "preparing the directory"); err != nil {
		return nil, err
	}
	if err := r.exec(ctx, conn, "umask 077 && cat > "+Quote(path.Join(dir, "env")), strings.NewReader(r.envFile(t, dir)), "scrittura dell'ambiente"); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(r.secrets)) {
		if err := r.exec(ctx, conn, "umask 077 && cat > "+Quote(path.Join(dir, "secrets", name)), strings.NewReader(r.secrets[name]), "scrittura dei segreti"); err != nil {
			return nil, err
		}
	}
	if want := r.action.SHA256; want != "" {
		if err := r.verifyBinary(ctx, conn, args[0], want); err != nil {
			return nil, err
		}
	}

	timeout := int(r.s.doc.Timeout / time.Second)
	if dl, ok := ctx.Deadline(); ok {
		timeout = max(1, int(time.Until(dl)/time.Second))
	}
	cmd := "/bin/sh -c " + Quote(fmt.Sprintf(script, timeout)) + " manent " + Quote(dir)
	for _, a := range args {
		cmd += " " + Quote(a)
	}
	attrs := []any{"target", t.Host}
	stdout := &lineWriter{ctx: ctx, log: r.log, red: r.red, budget: budget, attrs: append(attrs, "stream", "stdout")}
	stderr := &lineWriter{ctx: ctx, log: r.log, red: r.red, budget: budget, attrs: append(attrs, "stream", "stderr")}
	r.log.InfoContext(ctx, "comando avviato", "target", t.Host, "command", args[0], "args", len(args)-1, "dry_run", r.dry)
	code, err := conn.Exec(ctx, cmd, nil, stdout, stderr)
	stdout.flush()
	stderr.flush()
	switch {
	case ctx.Err() != nil:
		return nil, temporary("command interrupted: %w", ctx.Err())
	case err != nil:
		return nil, temporary("running the command: %w", err)
	case code == exitTimeout:
		return nil, temporary("command terminated by timeout after %ds", timeout)
	case code == ExitTempFail:
		return nil, temporary("the command asked for a retry (exit %d)", code)
	case code != 0:
		return nil, permanent("the command exited with code %d", code)
	}
	r.log.InfoContext(ctx, "comando completato", "target", t.Host)
	if len(r.s.doc.Outputs) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, left: maxOutputFile + 1}
	if code, err := conn.Exec(ctx, "cat -- "+Quote(path.Join(dir, "output")), nil, w, io.Discard); err != nil || code != 0 {
		return nil, temporary("reading the outputs: code %d, %v", code, err)
	}
	if buf.Len() > maxOutputFile {
		return nil, permanent("output over %d bytes", maxOutputFile)
	}
	return parseOutputs(buf.String())
}

// exec runs a service command; a non-zero code is a permanent error with the given
// description (for example secrets_dir not writable).
func (r *run) exec(ctx context.Context, conn Conn, cmd string, stdin io.Reader, what string) error {
	var stderr bytes.Buffer
	code, err := conn.Exec(ctx, cmd, stdin, io.Discard, &limitedWriter{w: &stderr, left: 2048})
	switch {
	case err != nil:
		return temporary("%s: %w", what, err)
	case code != 0:
		return permanent("%s: code %d: %s", what, code, strings.TrimSpace(r.red.apply(stderr.String())))
	}
	return nil
}

// verifyBinary compares the sha256 of the binary on the target with the spec's.
func (r *run) verifyBinary(ctx context.Context, conn Conn, bin, want string) error {
	var out bytes.Buffer
	q := Quote(bin)
	cmd := "if command -v sha256sum >/dev/null 2>&1; then sha256sum -- " + q + "; else shasum -a 256 -- " + q + "; fi"
	code, err := conn.Exec(ctx, cmd, nil, &limitedWriter{w: &out, left: 4096}, io.Discard)
	if err != nil {
		return temporary("checking the binary: %w", err)
	}
	got, _, _ := strings.Cut(strings.TrimSpace(out.String()), " ")
	if code != 0 || !strings.EqualFold(got, want) {
		return permanent("the sha256 of %s on the target does not match the one of the specification", bin)
	}
	return nil
}

// envFile produces the MANENT_* variables in the format of a file read by sh.
func (r *run) envFile(t descriptor.Target, dir string) string {
	vars := map[string]string{
		"MANENT_DEPLOYMENT_ID":   r.in.DeploymentID,
		"MANENT_STEP":            r.s.ps.Name,
		"MANENT_APPLICATION":     r.d.Application,
		"MANENT_ENVIRONMENT":     r.d.Environment,
		"MANENT_VERSION":         r.d.Artifact.Version,
		"MANENT_ARTIFACT_SHA256": r.data.SHA256,
		"MANENT_DRY_RUN":         strconv.FormatBool(r.dry),
		"MANENT_TARGET":          t.Host,
		"MANENT_SECRETS_DIR":     path.Join(dir, "secrets"),
		"MANENT_OUTPUT":          path.Join(dir, "output"),
	}
	for k, v := range r.data.Params {
		vars["MANENT_PARAM_"+envName(k)] = v
	}
	for step, out := range r.in.Outputs {
		for k, v := range out {
			vars["MANENT_OUTPUT_"+envName(step)+"_"+envName(k)] = v
		}
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if strings.ContainsRune(vars[k], 0) {
			continue
		}
		b.WriteString(k + "=" + Quote(vars[k]) + "\n")
	}
	return b.String()
}

var envUnsafe = regexp.MustCompile(`[^A-Z0-9]+`)

// envName turns a name into part of a variable name (post-test → POST_TEST).
func envName(s string) string { return envUnsafe.ReplaceAllString(strings.ToUpper(s), "_") }

// pathSafe makes an ID usable as a path component.
func pathSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, strings.TrimLeft(s, "."))
}

// Quote quotes an argument for a POSIX shell: in single quotes, with ' as '\”.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseOutputs reads the key=value lines of the outputs file.
func parseOutputs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, permanent("invalid output: key=value lines")
		}
		if _, dup := out[k]; dup {
			return nil, permanent("output %q repeated", k)
		}
		out[k] = v
	}
	return out, nil
}

// limitedWriter discards whatever exceeds the limit.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left <= 0 {
		return n, nil
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	l.left -= len(p)
	if _, err := l.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// nonce tells apart the directory of an attempt.
func nonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("random generator not available"))
	}
	return hex.EncodeToString(b)
}
