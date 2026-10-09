package remotecopy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// SSHDialer apre sessioni SSH/SFTP reali.
type SSHDialer struct {
	KnownHostsPath        string
	InsecureIgnoreHostKey bool
}

// Dial si connette al target autenticandosi con chiave privata e/o password.
func (d *SSHDialer) Dial(ctx context.Context, t plugin.Target, auth Auth) (Session, error) {
	hostKey, err := d.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	var methods []ssh.AuthMethod
	if auth.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(auth.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("chiave privata non valida: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if auth.Password != "" {
		methods = append(methods, ssh.Password(auth.Password))
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            t.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	sc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &sshSession{client: client, sftp: sc}, nil
}

func (d *SSHDialer) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if d.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	p := d.KnownHostsPath
	switch {
	case p == "":
		p = filepath.Join(home, ".ssh", "known_hosts")
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	}
	return knownhosts.New(p)
}

// sshSession implementa Session su una connessione SSH.
type sshSession struct {
	client *ssh.Client
	sftp   *sftp.Client
}

// Upload copia il file locale sul percorso remoto passando da un file temporaneo,
// così un'interruzione non lascia un artefatto parziale al posto di quello definitivo.
func (s *sshSession) Upload(ctx context.Context, localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := remotePath + ".part"
	dst, err := s.sftp.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, contextReader{ctx: ctx, r: src})
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return s.sftp.PosixRename(tmp, remotePath)
}

// Run esegue un comando remoto restituendo stdout; in caso di errore include stderr.
func (s *sshSession) Run(ctx context.Context, cmd string) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return stdout.String(), fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
		}
		return stdout.String(), nil
	}
}

// Close chiude SFTP e SSH.
func (s *sshSession) Close() error {
	s.sftp.Close()
	return s.client.Close()
}

// contextReader interrompe la copia quando il contesto viene cancellato.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
