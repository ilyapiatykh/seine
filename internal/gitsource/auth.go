// Package gitsource wraps go-git to expose a small "fetch the spec file from
// remote ref" surface that both the management server and agents share.
//
// The package is intentionally narrow: it does not expose the full go-git
// repository abstraction. Callers receive a Snapshot consisting of a commit
// SHA and the raw bytes of the configured file path, which is exactly what
// reconciliation loops need.
package gitsource

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	sshauth "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Auth describes how to authenticate against the remote. Use one of the
// concrete types below or nil for public unauthenticated repositories.
type Auth interface {
	build() (transport.AuthMethod, error)
}

// NoAuth is the explicit zero value. Equivalent to passing nil.
type NoAuth struct{}

func (NoAuth) build() (transport.AuthMethod, error) { return nil, nil }

// TokenAuth uses HTTPS basic auth with a token. The username is irrelevant
// for most providers (GitLab/GitHub accept anything for token auth) but a
// non-empty value is required by the protocol; we default to "git".
type TokenAuth struct {
	Username string // optional; defaults to "git"
	Token    string
}

func (t TokenAuth) build() (transport.AuthMethod, error) {
	if t.Token == "" {
		return nil, errors.New("gitsource: TokenAuth.Token is empty")
	}
	user := t.Username
	if user == "" {
		user = "git"
	}
	return &httpauth.BasicAuth{Username: user, Password: t.Token}, nil
}

// SSHKeyAuth authenticates using an OpenSSH private key on disk.
type SSHKeyAuth struct {
	User           string // typically "git"
	PrivateKeyPath string
	Passphrase     string // optional
}

func (s SSHKeyAuth) build() (transport.AuthMethod, error) {
	if s.PrivateKeyPath == "" {
		return nil, errors.New("gitsource: SSHKeyAuth.PrivateKeyPath is empty")
	}
	user := s.User
	if user == "" {
		user = "git"
	}
	if _, err := os.Stat(s.PrivateKeyPath); err != nil {
		return nil, fmt.Errorf("gitsource: ssh key %s: %w", s.PrivateKeyPath, err)
	}
	return sshauth.NewPublicKeysFromFile(user, s.PrivateKeyPath, s.Passphrase)
}
