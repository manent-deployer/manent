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

	"github.com/manent-deployer/manent/pkg/plugin"
)

// SSHDialer opens real SSH/SFTP sessions.
type SSHDialer struct {
	KnownHostsPath        string
	InsecureIgnoreHostKey bool
}

// Dial connects to the target authenticating with a private key and/or password.
func (d *SSHDialer) Dial(ctx context.Context, t plugin.Target, auth Auth) (Session, error) {
	hostKey, err := d.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	var methods []ssh.AuthMethod
	if auth.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(auth.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %w", err)
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

// sshSession implements Session on an SSH connection.
type sshSession struct {
	client *ssh.Client
	sftp   *sftp.Client
}

// Upload copies the local file to the remote path through a temporary file, so an
// interruption does not leave a partial artifact in place of the final one.
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

// Run runs a remote command returning stdout; on error it includes stderr.
func (s *sshSession) Run(ctx context.Context, cmd string) (string, error) {
	return s.RunStdin(ctx, cmd, nil)
}

// RunStdin runs a remote command passing it stdin (e.g. a password, which so does not show up
// in the command line visible on the target).
func (s *sshSession) RunStdin(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr, sess.Stdin = &stdout, &stderr, stdin
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

// Close closes SFTP and SSH.
func (s *sshSession) Close() error {
	s.sftp.Close()
	return s.client.Close()
}

// contextReader stops the copy when the context is cancelled.
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
