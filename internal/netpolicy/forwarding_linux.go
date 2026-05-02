//go:build linux

package netpolicy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ilyapiatykh/seine/internal/logging"
)

const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// EnsureIPForwarding makes sure net.ipv4.ip_forward is 1, which is the
// kernel switch that lets a hub forward packets between WireGuard peers
// on the same interface. The function is idempotent and is safe to call
// repeatedly.
//
// In a containerised demo, ip_forward is sometimes already enabled by the
// orchestrator (Docker sets it to 1 by default on bridge networks); we
// detect that case and short-circuit, so the call works inside containers
// where /proc/sys is read-only.
func EnsureIPForwarding(ctx context.Context) error {
	current, err := os.ReadFile(ipForwardPath)
	if err != nil {
		return fmt.Errorf("netpolicy: read %s: %w", ipForwardPath, err)
	}
	if strings.TrimSpace(string(current)) == "1" {
		logging.FromContext(ctx).Debug("ip_forward already enabled",
			slog.String("component", "netpolicy"))
		return nil
	}
	if err := os.WriteFile(ipForwardPath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("netpolicy: write %s (try running with --privileged or --sysctl net.ipv4.ip_forward=1): %w",
			ipForwardPath, err)
	}
	logging.FromContext(ctx).Info("ip_forward enabled",
		slog.String("component", "netpolicy"))
	return nil
}
