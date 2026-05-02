// Package wg manages the local WireGuard tunnel: it owns the agent's
// keypair, drives interface up/down, and reconciles the peer set against a
// desired configuration computed by the agent's reconciliation loop.
//
// The package exposes a single Interface abstraction. On Linux it is
// implemented on top of the kernel WireGuard module via wgctrl + netlink;
// on other operating systems the implementation is a stub returning an
// "unsupported OS" error. Cross-platform support via a userspace TUN
// (wireguard-go) is a planned future expansion — the abstraction is shaped
// to admit a second backend without changes to callers.
package wg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Keypair is a WireGuard keypair on the agent. The private key is what is
// loaded into the interface; the public key is what we publish to the
// control plane.
type Keypair struct {
	Private wgtypes.Key
	Public  wgtypes.Key
}

// GenerateKeypair returns a freshly generated keypair using Curve25519.
func GenerateKeypair() (Keypair, error) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return Keypair{}, fmt.Errorf("wg: generate private key: %w", err)
	}
	return Keypair{Private: priv, Public: priv.PublicKey()}, nil
}

// LoadOrGenerate reads a private key from path; if absent, generates a new
// one and writes it. The file is created with 0600 permissions.
func LoadOrGenerate(path string) (Keypair, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, perr := parseKey(string(data))
		if perr != nil {
			return Keypair{}, fmt.Errorf("wg: parse private key %s: %w", path, perr)
		}
		return Keypair{Private: key, Public: key.PublicKey()}, nil
	case errors.Is(err, os.ErrNotExist):
		// fall through to generation
	default:
		return Keypair{}, fmt.Errorf("wg: read private key %s: %w", path, err)
	}

	kp, err := GenerateKeypair()
	if err != nil {
		return Keypair{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Keypair{}, fmt.Errorf("wg: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(kp.Private.String()+"\n"), 0o600); err != nil {
		return Keypair{}, fmt.Errorf("wg: write private key %s: %w", path, err)
	}
	return kp, nil
}

// parseKey accepts either a base64 key (canonical wg-quick form) or that
// form with surrounding whitespace.
func parseKey(s string) (wgtypes.Key, error) {
	s = trim(s)
	return wgtypes.ParseKey(s)
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
