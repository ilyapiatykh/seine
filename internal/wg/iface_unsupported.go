//go:build !linux

package wg

import (
	"errors"
	"fmt"
	"runtime"
)

// newPlatformInterface returns an error on non-Linux builds. The agent's
// reconciliation loop surfaces this through normal logging, so an operator
// running the binary on macOS/Windows sees a clear "unsupported OS"
// message rather than a misleading runtime crash. A userspace TUN backend
// based on wireguard-go is a planned expansion.
func newPlatformInterface(name string) (Interface, error) {
	return nil, fmt.Errorf("wg: %w (running on %s)", errUnsupported, runtime.GOOS)
}

var errUnsupported = errors.New("only Linux is supported in this build")
