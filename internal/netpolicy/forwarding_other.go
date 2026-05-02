//go:build !linux

package netpolicy

import (
	"context"
	"errors"
	"runtime"
)

// EnsureIPForwarding is unsupported off Linux; the agent should refuse to
// start as a hub on other systems.
func EnsureIPForwarding(_ context.Context) error {
	return errors.New("netpolicy: hub IP forwarding is only supported on Linux (running on " + runtime.GOOS + ")")
}
