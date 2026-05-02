package gitsource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/ilyapiatykh/seine/internal/logging"
)

// Config configures a Source. URL, Branch and Path are required.
type Config struct {
	// URL of the remote (https://, ssh:// or file://).
	URL string

	// Branch is the short name of the ref the agent tracks (e.g. "main").
	Branch string

	// Path is the file inside the working tree to extract on each Pull,
	// for example "network.yaml" or "envs/prod/network.yaml".
	Path string

	// Workdir is the local cache directory. If empty, an in-process
	// temporary directory is created and removed on Close.
	Workdir string

	// Auth is the authentication method. nil or NoAuth{} for public
	// repositories.
	Auth Auth
}

// Source is a configured Git remote that yields snapshots of one file.
type Source struct {
	cfg     Config
	workdir string
	owned   bool // true if we created workdir and should remove it on Close
}

// Snapshot is the result of a successful Pull: the commit currently at the
// tracked branch tip plus the raw bytes of the configured file.
type Snapshot struct {
	CommitSHA string
	Branch    string
	Path      string
	Data      []byte
}

// Open prepares a Source. The remote is not contacted until Pull.
func Open(cfg Config) (*Source, error) {
	if cfg.URL == "" {
		return nil, errors.New("gitsource: URL is required")
	}
	if cfg.Branch == "" {
		return nil, errors.New("gitsource: Branch is required")
	}
	if cfg.Path == "" {
		return nil, errors.New("gitsource: Path is required")
	}
	s := &Source{cfg: cfg, workdir: cfg.Workdir}
	if s.workdir == "" {
		dir, err := os.MkdirTemp("", "seine-git-")
		if err != nil {
			return nil, fmt.Errorf("gitsource: mktemp: %w", err)
		}
		s.workdir = dir
		s.owned = true
	} else if err := os.MkdirAll(s.workdir, 0o755); err != nil {
		return nil, fmt.Errorf("gitsource: mkdir %s: %w", s.workdir, err)
	}
	return s, nil
}

// Close releases temporary resources. Safe to call multiple times.
func (s *Source) Close() error {
	if s == nil || !s.owned || s.workdir == "" {
		return nil
	}
	err := os.RemoveAll(s.workdir)
	s.workdir = ""
	s.owned = false
	return err
}

// Workdir is the on-disk path where the repository is cached.
func (s *Source) Workdir() string { return s.workdir }

// Pull synchronises the local cache with the remote and returns a snapshot
// of the configured file at the tip of the tracked branch.
func (s *Source) Pull(ctx context.Context) (*Snapshot, error) {
	auth, err := resolveAuth(s.cfg.Auth)
	if err != nil {
		return nil, err
	}

	repo, err := s.openOrClone(ctx, auth)
	if err != nil {
		return nil, err
	}

	if err := s.fetch(ctx, repo, auth); err != nil {
		return nil, err
	}

	remoteRef := plumbing.NewRemoteReferenceName("origin", s.cfg.Branch)
	ref, err := repo.Reference(remoteRef, true)
	if err != nil {
		return nil, fmt.Errorf("gitsource: resolve %s: %w", remoteRef, err)
	}
	hash := ref.Hash()
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("gitsource: load commit %s: %w", hash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("gitsource: tree %s: %w", hash, err)
	}
	file, err := tree.File(s.cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("gitsource: file %q at %s: %w", s.cfg.Path, hash, err)
	}
	contents, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("gitsource: read %q: %w", s.cfg.Path, err)
	}

	return &Snapshot{
		CommitSHA: hash.String(),
		Branch:    s.cfg.Branch,
		Path:      s.cfg.Path,
		Data:      []byte(contents),
	}, nil
}

func (s *Source) openOrClone(ctx context.Context, auth transport.AuthMethod) (*git.Repository, error) {
	log := logging.FromContext(ctx).With(slog.String("component", "gitsource"))

	if _, err := os.Stat(filepath.Join(s.workdir, ".git")); err == nil {
		repo, err := git.PlainOpen(s.workdir)
		if err != nil {
			return nil, fmt.Errorf("gitsource: open existing repo: %w", err)
		}
		return repo, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	log.Info("cloning git source",
		slog.String("url", s.cfg.URL),
		slog.String("branch", s.cfg.Branch),
		slog.String("workdir", s.workdir),
	)
	repo, err := git.PlainCloneContext(ctx, s.workdir, false, &git.CloneOptions{
		URL:           s.cfg.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(s.cfg.Branch),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("gitsource: clone %s: %w", s.cfg.URL, err)
	}
	return repo, nil
}

func (s *Source) fetch(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) error {
	log := logging.FromContext(ctx).With(slog.String("component", "gitsource"))
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s",
				s.cfg.Branch, s.cfg.Branch)),
		},
		Force: true,
	})
	switch {
	case err == nil:
		log.Debug("git fetch updated remote ref")
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		// Common case; not an error.
	default:
		return fmt.Errorf("gitsource: fetch: %w", err)
	}
	return nil
}

// resolveAuth turns a possibly-nil Auth into the go-git transport method.
func resolveAuth(a Auth) (transport.AuthMethod, error) {
	if a == nil {
		return nil, nil
	}
	return a.build()
}
