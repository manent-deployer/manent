package customstep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// SSHDialer apre connessioni SSH reali, con verifica della host key.
type SSHDialer struct {
	KnownHostsPath        string
	InsecureIgnoreHostKey bool
}

// Dial si connette al target con chiave privata e/o password.
func (d *SSHDialer) Dial(ctx context.Context, t descriptor.Target, user string, auth SSHAuth) (Conn, error) {
	hostKey, err := d.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	var methods []ssh.AuthMethod
	if auth.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(auth.PrivateKey))
		if err != nil {
			return nil, errors.New("chiave privata SSH non valida")
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if auth.Password != "" {
		methods = append(methods, ssh.Password(strings.TrimRight(auth.Password, "\r\n")))
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: user, Auth: methods, HostKeyCallback: hostKey})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &sshConn{client: ssh.NewClient(c, chans, reqs)}, nil
}

func (d *SSHDialer) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if d.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	p := d.KnownHostsPath
	if p == "" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if p == "" {
			p = filepath.Join(home, ".ssh", "known_hosts")
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	return knownhosts.New(p)
}

type sshConn struct{ client *ssh.Client }

// Exec esegue il comando in una nuova sessione. Allo scadere del contesto invia SIGKILL e
// chiude la sessione; molti server ignorano i segnali, per questo lo script usa timeout(1).
func (c *sshConn) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		<-done
		return -1, ctx.Err()
	case err := <-done:
		var exit *ssh.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &exit):
			if exit.Signal() != "" {
				return -1, fmt.Errorf("comando terminato dal segnale %s", exit.Signal())
			}
			return exit.ExitStatus(), nil
		}
		return -1, err
	}
}

func (c *sshConn) Close() error { return c.client.Close() }
