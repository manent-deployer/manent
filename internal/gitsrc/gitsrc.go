// Package gitsrc reads files at a commit of a Git repository: it keeps a bare copy on disk,
// resolves refs into commit SHAs, checks that a SHA is reachable from the allowed refs and
// verifies commit signatures (OpenPGP or SSH). It uses go-git: the server image has no git
// binary.
package gitsrc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// ErrNotFound means the file does not exist at the commit.
var ErrNotFound = errors.New("file not found in the repository")

// Secrets resolves the references to the credentials.
type Secrets interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Options configures a repository.
type Options struct {
	// Repository is the URL (https://, ssh://, git://, file://) or a local path.
	Repository string
	// DefaultRef is the ref read when none is requested (e.g. refs/heads/main).
	DefaultRef string
	// AllowedRefs are the refs that can be read and from which a SHA must be reachable,
	// patterns with *; the default ref is always allowed.
	AllowedRefs []string
	// Username and PasswordRef are the HTTPS credentials (the password is a secret reference).
	Username    string
	PasswordRef string
	// SSHKeyRef is the reference to the SSH private key; KnownHosts is required with it.
	SSHKeyRef  string
	KnownHosts string
	// CAFile is an additional CA bundle for HTTPS.
	CAFile string
	// RequireSigned refuses commits without a valid signature from the signers below.
	RequireSigned bool
	// OpenPGPKeyring is an armored keyring; SSHAllowedSigners an allowed_signers file.
	OpenPGPKeyring    string
	SSHAllowedSigners string
	// MaxAge refuses commits older than this (0 = no limit), except pinned revisions.
	MaxAge time.Duration
	// CacheDir holds the bare copies, one per repository URL.
	CacheDir string
	// RefTTL is how long a ref resolution is reused before fetching again.
	RefTTL  time.Duration
	Secrets Secrets
	// Now is the clock (tests).
	Now func() time.Time
}

// Commit is a resolved revision.
type Commit struct {
	SHA string
	// Ref is the ref the SHA was resolved from; empty when a SHA was requested.
	Ref  string
	Time time.Time
}

// Repo is a repository read through a local bare copy.
type Repo struct {
	opt     Options
	allowed []string
	mu      sync.Mutex
	repo    *git.Repository
	fetched time.Time
}

var (
	shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/*-]*$`)
)

// IsSHA says whether s is a full commit SHA (SHA-1 or SHA-256).
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// ValidRef says whether s can be a ref name or a SHA.
func ValidRef(s string) bool {
	return refRe.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "*") && !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock")
}

// New checks the options; the repository is opened or cloned at the first read.
func New(opt Options) (*Repo, error) {
	if opt.Repository == "" {
		return nil, errors.New("git: repository is required")
	}
	if opt.DefaultRef == "" {
		opt.DefaultRef = "refs/heads/main"
	}
	if !strings.HasPrefix(opt.DefaultRef, "refs/") || !ValidRef(opt.DefaultRef) {
		return nil, fmt.Errorf("git: ref %q must be a full ref name (refs/heads/..., refs/tags/...)", opt.DefaultRef)
	}
	for _, p := range opt.AllowedRefs {
		if !strings.HasPrefix(p, "refs/") || !refRe.MatchString(p) {
			return nil, fmt.Errorf("git: allowed_refs %q must be a full ref name or pattern (refs/tags/env-*)", p)
		}
	}
	if opt.SSHKeyRef != "" && opt.KnownHosts == "" {
		return nil, errors.New("git: ssh_key_ref requires known_hosts")
	}
	if opt.SSHKeyRef != "" && opt.PasswordRef != "" {
		return nil, errors.New("git: password_ref and ssh_key_ref are alternative")
	}
	if opt.RequireSigned && opt.OpenPGPKeyring == "" && opt.SSHAllowedSigners == "" {
		return nil, errors.New("git: require_signed needs signers.openpgp_keyring or signers.ssh_allowed_signers")
	}
	if opt.CacheDir == "" {
		return nil, errors.New("git: cache_dir is required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Repo{opt: opt, allowed: append([]string{opt.DefaultRef}, opt.AllowedRefs...)}, nil
}

// URL returns the repository URL without credentials, for sources and audit.
func (r *Repo) URL() string { return redact(r.opt.Repository) }

// redact removes the user information from a URL.
func redact(u string) string {
	if scheme, rest, ok := strings.Cut(u, "://"); ok {
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			return scheme + "://" + rest[at+1:]
		}
	}
	return u
}

// Resolve turns want (empty = default ref, a ref name or a SHA) into a commit, checking that it
// is allowed, signed when required and not older than max_age (pinned revisions are exempt: a
// rollback goes back to a recorded revision). Network errors are transient, the others
// permanent.
func (r *Repo) Resolve(ctx context.Context, want string, pinned bool) (Commit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if want == "" {
		want = r.opt.DefaultRef
	}
	if !ValidRef(want) {
		return Commit{}, pipeline.Permanent(fmt.Errorf("invalid config ref %q", want))
	}
	if err := r.open(ctx); err != nil {
		return Commit{}, err
	}
	var c Commit
	var err error
	if IsSHA(want) {
		c, err = r.resolveSHA(ctx, want)
	} else {
		c, err = r.resolveRef(ctx, want)
	}
	if err != nil {
		return Commit{}, err
	}
	commit, err := r.repo.CommitObject(plumbing.NewHash(c.SHA))
	if err != nil {
		return Commit{}, pipeline.Permanent(fmt.Errorf("config revision %s: %w", c.SHA, err))
	}
	c.Time = commit.Committer.When
	if r.opt.MaxAge > 0 && !pinned && r.opt.Now().Sub(c.Time) > r.opt.MaxAge {
		return Commit{}, pipeline.Permanent(fmt.Errorf("config revision %s is older than max_age (%s)", short(c.SHA), r.opt.MaxAge))
	}
	if r.opt.RequireSigned {
		if err := r.verify(commit); err != nil {
			return Commit{}, pipeline.Permanent(fmt.Errorf("config revision %s: %w", short(c.SHA), err))
		}
	}
	return c, nil
}

// Read returns the file at path in the commit sha (resolved by Resolve).
func (r *Repo) Read(ctx context.Context, sha, file string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	clean := path.Clean("/" + file)[1:]
	if clean == "" || clean != strings.TrimPrefix(file, "./") {
		return nil, pipeline.Permanent(fmt.Errorf("invalid path %q in the repository", file))
	}
	if err := r.open(ctx); err != nil {
		return nil, err
	}
	commit, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, pipeline.Permanent(fmt.Errorf("config revision %s: %w", sha, err))
	}
	f, err := commit.File(clean)
	if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return nil, fmt.Errorf("%w: %s@%s:%s", ErrNotFound, r.URL(), short(sha), clean)
	}
	if err != nil {
		return nil, err
	}
	rd, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(io.LimitReader(rd, 1<<20))
}

// open opens the bare copy, creating it on first use.
func (r *Repo) open(context.Context) error {
	if r.repo != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(r.opt.Repository))
	dir := filepath.Join(r.opt.CacheDir, hex.EncodeToString(sum[:8])+".git")
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if err := os.MkdirAll(r.opt.CacheDir, 0o700); err != nil {
			return err
		}
		repo, err = git.PlainInit(dir, true)
		if err == nil {
			_, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{r.opt.Repository}})
		}
	}
	if err != nil {
		return fmt.Errorf("git cache %s: %w", dir, err)
	}
	r.repo = repo
	return nil
}

// fetch updates branches and tags from the repository and removes the local refs that no longer
// exist there, so a deleted allowed branch stops counting for reachability.
func (r *Repo) fetch(ctx context.Context) error {
	auth, err := r.auth(ctx)
	if err != nil {
		return err
	}
	var ca []byte
	if r.opt.CAFile != "" {
		if ca, err = os.ReadFile(r.opt.CAFile); err != nil {
			return pipeline.Permanent(fmt.Errorf("git ca_file: %w", err))
		}
	}
	remote, err := r.repo.Remote("origin")
	if err != nil {
		return err
	}
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []gitconfig.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
		Auth:     auth, Tags: git.NoTags, Force: true, CABundle: ca,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return r.fetchError(err)
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth, CABundle: ca})
	if err != nil {
		return r.fetchError(err)
	}
	live := map[plumbing.ReferenceName]bool{}
	for _, ref := range refs {
		live[ref.Name()] = true
	}
	iter, err := r.repo.References()
	if err != nil {
		return err
	}
	var stale []plumbing.ReferenceName
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if n := ref.Name(); (n.IsBranch() || n.IsTag()) && !live[n] {
			stale = append(stale, n)
		}
		return nil
	})
	for _, n := range stale {
		_ = r.repo.Storer.RemoveReference(n)
	}
	r.fetched = r.opt.Now()
	return nil
}

// fetchError keeps network errors transient; authentication and missing repositories are
// configuration errors.
func (r *Repo) fetchError(err error) error {
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) || errors.Is(err, transport.ErrRepositoryNotFound) {
		return pipeline.Permanent(fmt.Errorf("git fetch %s: %w", r.URL(), err))
	}
	return fmt.Errorf("git fetch %s: %w", r.URL(), err)
}

// auth builds the credentials, resolved at every fetch and kept only in memory.
func (r *Repo) auth(ctx context.Context) (transport.AuthMethod, error) {
	user := r.opt.Username
	switch {
	case r.opt.PasswordRef != "":
		secret, err := r.opt.Secrets.Resolve(ctx, r.opt.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("git password_ref: %w", err)
		}
		if user == "" {
			user = "git"
		}
		return &githttp.BasicAuth{Username: user, Password: secret}, nil
	case r.opt.SSHKeyRef != "":
		key, err := r.opt.Secrets.Resolve(ctx, r.opt.SSHKeyRef)
		if err != nil {
			return nil, fmt.Errorf("git ssh_key_ref: %w", err)
		}
		if user == "" {
			user = "git"
		}
		pk, err := gitssh.NewPublicKeys(user, []byte(key), "")
		if err != nil {
			return nil, pipeline.Permanent(fmt.Errorf("git ssh_key_ref: %w", err))
		}
		cb, err := gitssh.NewKnownHostsCallback(r.opt.KnownHosts)
		if err != nil {
			return nil, pipeline.Permanent(fmt.Errorf("git known_hosts: %w", err))
		}
		pk.HostKeyCallback = cb
		return pk, nil
	}
	return nil, nil
}

// isAllowed says whether the full ref name matches the allowed refs.
func (r *Repo) isAllowed(name string) bool {
	return slices.ContainsFunc(r.allowed, func(p string) bool {
		ok, _ := path.Match(p, name)
		return ok || p == name
	})
}

// candidates are the full names a short ref can stand for.
func candidates(want string) []string {
	if strings.HasPrefix(want, "refs/") {
		return []string{want}
	}
	return []string{"refs/heads/" + want, "refs/tags/" + want}
}

// resolveRef resolves an allowed ref, fetching when the last fetch is older than ref_ttl.
func (r *Repo) resolveRef(ctx context.Context, want string) (Commit, error) {
	names := slices.DeleteFunc(candidates(want), func(n string) bool { return !r.isAllowed(n) })
	if len(names) == 0 {
		return Commit{}, pipeline.Permanent(fmt.Errorf("config ref %s is not allowed (allowed_refs)", want))
	}
	if r.fetched.IsZero() || r.opt.Now().Sub(r.fetched) >= r.opt.RefTTL {
		if err := r.fetch(ctx); err != nil {
			return Commit{}, err
		}
	}
	for _, n := range names {
		ref, err := r.repo.Reference(plumbing.ReferenceName(n), true)
		if err != nil {
			continue
		}
		h, err := r.peel(ref.Hash())
		if err != nil {
			return Commit{}, pipeline.Permanent(fmt.Errorf("config ref %s: %w", n, err))
		}
		return Commit{SHA: h.String(), Ref: n}, nil
	}
	return Commit{}, pipeline.Permanent(fmt.Errorf("config ref %s does not exist in %s", want, r.URL()))
}

// peel follows annotated tags to the commit.
func (r *Repo) peel(h plumbing.Hash) (plumbing.Hash, error) {
	for range 10 {
		tag, err := r.repo.TagObject(h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return h, nil
		}
		if err != nil {
			return h, err
		}
		h = tag.Target
	}
	return h, errors.New("tag chain too long")
}

// resolveSHA checks that the commit exists and is reachable from an allowed ref, fetching once
// when it is missing or not yet reachable locally.
func (r *Repo) resolveSHA(ctx context.Context, sha string) (Commit, error) {
	for attempt := 0; ; attempt++ {
		ok, err := r.reachable(sha)
		if err != nil {
			return Commit{}, err
		}
		if ok {
			return Commit{SHA: sha}, nil
		}
		if attempt > 0 {
			return Commit{}, pipeline.Permanent(fmt.Errorf("config revision %s is not reachable from the allowed refs (allowed_refs)", short(sha)))
		}
		if err := r.fetch(ctx); err != nil {
			return Commit{}, err
		}
	}
}

// reachable says whether sha is a local commit and an ancestor of (or equal to) an allowed ref.
func (r *Repo) reachable(sha string) (bool, error) {
	commit, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	iter, err := r.repo.References()
	if err != nil {
		return false, err
	}
	defer iter.Close()
	found := false
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if found || ref.Type() != plumbing.HashReference || !r.isAllowed(ref.Name().String()) {
			return nil
		}
		h, err := r.peel(ref.Hash())
		if err != nil {
			return nil
		}
		tip, err := r.repo.CommitObject(h)
		if err != nil {
			return nil
		}
		if tip.Hash == commit.Hash {
			found = true
			return nil
		}
		if ok, _ := commit.IsAncestor(tip); ok {
			found = true
		}
		return nil
	})
	return found, err
}

// short abbreviates a SHA for messages.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
