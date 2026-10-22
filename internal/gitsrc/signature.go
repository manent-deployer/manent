package gitsrc

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"
)

// sshNamespace is the namespace git uses for SSH signatures of commits and tags.
const sshNamespace = "git"

// verify checks the commit signature against the configured signers. The signer files are read
// at every verification, so a rotated key needs no restart.
func (r *Repo) verify(c *object.Commit) error {
	sig := strings.TrimSpace(c.PGPSignature)
	switch {
	case sig == "":
		return errors.New("commit is not signed (require_signed)")
	case strings.HasPrefix(sig, "-----BEGIN PGP SIGNATURE-----"):
		if r.opt.OpenPGPKeyring == "" {
			return errors.New("OpenPGP signature but no signers.openpgp_keyring")
		}
		keyring, err := os.ReadFile(r.opt.OpenPGPKeyring)
		if err != nil {
			return fmt.Errorf("signers.openpgp_keyring: %w", err)
		}
		if _, err := c.Verify(string(keyring)); err != nil {
			return fmt.Errorf("invalid OpenPGP signature: %w", err)
		}
		return nil
	case strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----"):
		if r.opt.SSHAllowedSigners == "" {
			return errors.New("SSH signature but no signers.ssh_allowed_signers")
		}
		keys, err := readAllowedSigners(r.opt.SSHAllowedSigners)
		if err != nil {
			return fmt.Errorf("signers.ssh_allowed_signers: %w", err)
		}
		msg := &plumbing.MemoryObject{}
		if err := c.EncodeWithoutSignature(msg); err != nil {
			return err
		}
		rd, err := msg.Reader()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rd)
		if err != nil {
			return err
		}
		return verifySSH(sig, data, keys)
	}
	return errors.New("unknown signature format")
}

// readAllowedSigners parses an allowed_signers file (ssh-keygen ALLOWED SIGNERS): principals,
// optional options, then the public key. Only keys usable for the git namespace are kept.
func readAllowedSigners(file string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var keys []ssh.PublicKey
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if i == 0 || !(strings.HasPrefix(f, "ssh-") || strings.HasPrefix(f, "ecdsa-") || strings.HasPrefix(f, "sk-")) {
				continue
			}
			if opts := strings.Join(fields[1:i], " "); strings.Contains(opts, "namespaces=") && !strings.Contains(opts, sshNamespace) {
				break
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[i:], " ")))
			if err != nil {
				return nil, fmt.Errorf("line %q: %w", line, err)
			}
			keys = append(keys, key)
			break
		}
	}
	return keys, sc.Err()
}

// verifySSH verifies an armored SSHSIG signature of message made by one of the keys.
func verifySSH(armored string, message []byte, keys []ssh.PublicKey) error {
	block, _ := pem.Decode([]byte(armored))
	if block == nil || block.Type != "SSH SIGNATURE" || !bytes.HasPrefix(block.Bytes, []byte("SSHSIG")) {
		return errors.New("invalid SSH signature")
	}
	var blob struct {
		Version       uint32
		PublicKey     []byte
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Signature     []byte
	}
	if err := ssh.Unmarshal(block.Bytes[6:], &blob); err != nil {
		return fmt.Errorf("invalid SSH signature: %w", err)
	}
	if blob.Version != 1 || blob.Namespace != sshNamespace {
		return fmt.Errorf("SSH signature with version %d and namespace %q, expected 1 and %q", blob.Version, blob.Namespace, sshNamespace)
	}
	pub, err := ssh.ParsePublicKey(blob.PublicKey)
	if err != nil {
		return fmt.Errorf("invalid SSH signature key: %w", err)
	}
	trusted := false
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), pub.Marshal()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return fmt.Errorf("SSH signature by %s, not in signers.ssh_allowed_signers", ssh.FingerprintSHA256(pub))
	}
	var hash []byte
	switch blob.HashAlgorithm {
	case "sha256":
		h := sha256.Sum256(message)
		hash = h[:]
	case "sha512":
		h := sha512.Sum512(message)
		hash = h[:]
	default:
		return fmt.Errorf("SSH signature hash %q not supported", blob.HashAlgorithm)
	}
	signed := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Hash          []byte
	}{blob.Namespace, blob.Reserved, blob.HashAlgorithm, hash})...)
	var sig ssh.Signature
	if err := ssh.Unmarshal(blob.Signature, &sig); err != nil {
		return fmt.Errorf("invalid SSH signature: %w", err)
	}
	if err := pub.Verify(signed, &sig); err != nil {
		return fmt.Errorf("invalid SSH signature: %w", err)
	}
	return nil
}
