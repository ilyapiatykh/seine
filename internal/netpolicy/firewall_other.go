//go:build !linux

package netpolicy

import (
	"context"
	"errors"
	"runtime"
)

// Firewall is the cross-platform stub. The hub-side firewall reconciler
// is iptables-only; on other systems calls return an explicit error so
// the agent fails fast.
type Firewall struct{}

// NewFirewall returns an error on non-Linux builds.
func NewFirewall(_ string) (*Firewall, error) {
	return nil, errors.New("netpolicy: hub firewall is only supported on Linux (running on " + runtime.GOOS + ")")
}

func (*Firewall) Reconcile(_ context.Context, _ *Compiled) error {
	return errors.New("netpolicy: firewall not initialised")
}

func (*Firewall) Teardown(_ context.Context) {}
