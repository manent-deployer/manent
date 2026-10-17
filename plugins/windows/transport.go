package windows

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/masterzen/winrm"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

// Transport runs PowerShell scripts on one Windows host.
type Transport interface {
	// Run runs the script and returns its stdout; data, if not nil, is sent to the script as
	// base64 lines after the script itself (see bootstrap). A non-zero exit is an error with
	// the stderr of the script.
	Run(ctx context.Context, script string, data io.Reader) (string, error)
	Close() error
}

// Connector opens transports to the targets; replaced in tests.
type Connector interface {
	Connect(ctx context.Context, t plugin.Target, c Conn) (Transport, error)
}

// Conn are the connection settings of a target, from the parameters and the secrets.
type Conn struct {
	Transport string // winrm or ssh
	// Shell is the PowerShell executable: powershell (Windows PowerShell 5.1) or pwsh.
	Shell string
	// WinRM: port, HTTPS, authentication (ntlm or basic) and certificate check.
	Port       int
	HTTPS      bool
	Auth       string
	Insecure   bool
	Password   string
	PrivateKey string
}

// bootstrap is the command line of every script, short enough for the cmd.exe limit (8191
// characters) whatever the script: it reads the script as the first stdin line (base64 of
// UTF-8) and runs it; the following lines stay on stdin for the data. A terminating error
// writes the message on stderr and exits with 1.
func bootstrap(shell string) string {
	ps := `$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'
trap { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }
$s = [Console]::In.ReadLine()
& ([scriptblock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($s))))
exit 0`
	return shell + " -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + encodeCommand(ps)
}

// encodeCommand encodes a script for -EncodedCommand: base64 of UTF-16LE.
func encodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// chunk is the size of the data read for one base64 line (a multiple of 3: no padding inside).
const chunk = 48 * 1024

// stdin returns the stdin of a script: the script line, then the data as base64 lines and an
// empty line as end.
func stdin(script string, data io.Reader) io.Reader {
	head := strings.NewReader(base64.StdEncoding.EncodeToString([]byte(script)) + "\n")
	if data == nil {
		return head
	}
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 2*chunk)
		buf := make([]byte, chunk)
		for {
			n, err := io.ReadFull(data, buf)
			if n > 0 {
				_, _ = w.WriteString(base64.StdEncoding.EncodeToString(buf[:n]) + "\n")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		_, _ = w.WriteString("\n")
		pw.CloseWithError(w.Flush())
	}()
	return io.MultiReader(head, pr)
}

// DefaultConnector connects with WinRM (github.com/masterzen/winrm) or with the OpenSSH server
// of Windows (the SSH dialer of remote-copy).
type DefaultConnector struct {
	SSH *remotecopy.SSHDialer
	// CACert is the PEM of the CA of the WinRM HTTPS certificates; empty: system CAs.
	CACert []byte
}

// Connect implements Connector.
func (d DefaultConnector) Connect(ctx context.Context, t plugin.Target, c Conn) (Transport, error) {
	if c.Transport == TransportSSH {
		sess, err := d.SSH.Dial(ctx, t, remotecopy.Auth{PrivateKey: c.PrivateKey, Password: c.Password})
		if err != nil {
			return nil, err
		}
		return &sshTransport{sess: sess, shell: c.Shell}, nil
	}
	port := c.Port
	if t.Port != 0 {
		port = t.Port
	}
	ep := winrm.NewEndpoint(t.Host, port, c.HTTPS, c.Insecure, d.CACert, nil, nil, 0)
	params := winrm.NewParameters("PT60S", "en-US", 153600)
	if c.Auth == AuthNTLM {
		params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	}
	client, err := winrm.NewClientWithParameters(ep, t.User, c.Password, params)
	if err != nil {
		return nil, fmt.Errorf("client WinRM: %w", err)
	}
	return &winrmTransport{client: client, shell: c.Shell}, nil
}

type sshTransport struct {
	sess  remotecopy.Session
	shell string
}

func (s *sshTransport) Run(ctx context.Context, script string, data io.Reader) (string, error) {
	sr, ok := s.sess.(remotecopy.StdinRunner)
	if !ok {
		return "", errors.New("sessione SSH senza stdin")
	}
	return sr.RunStdin(ctx, bootstrap(s.shell), stdin(script, data))
}

func (s *sshTransport) Close() error { return s.sess.Close() }

type winrmTransport struct {
	client *winrm.Client
	shell  string
}

func (w *winrmTransport) Run(ctx context.Context, script string, data io.Reader) (string, error) {
	var stdout, stderr bytes.Buffer
	code, err := w.client.RunWithContextWithInput(ctx, bootstrap(w.shell), &stdout, &stderr, stdin(script, data))
	if err != nil {
		return stdout.String(), fmt.Errorf("WinRM: %w", err)
	}
	if code != 0 {
		return stdout.String(), fmt.Errorf("uscita %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (w *winrmTransport) Close() error { return nil }

// results reads the MANENT:<key>=<value> lines written by the scripts; other output is ignored.
func results(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(strings.TrimPrefix(l, "MANENT:"), "="); ok && strings.HasPrefix(l, "MANENT:") {
			m[k] = v
		}
	}
	return m
}

// psQuote is a PowerShell single-quoted string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// readCA reads the WinRM CA file of the plugin configuration.
func readCA(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("winrm_ca_file: %w", err)
	}
	return b, nil
}

// seconds formats a timeout for the scripts.
func seconds(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) }
