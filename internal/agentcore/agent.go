// Package agentcore is the brain of the seine-agent binary.
//
// It owns the agent's WireGuard keypair and bearer token, performs the
// initial Register call against the management server, brings up the
// local WireGuard interface, and runs the reconciliation loop that keeps
// the kernel's peer set aligned with the desired state computed from the
// network spec and the runtime registry.
package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/buildinfo"
	"github.com/ilyapiatykh/seine/internal/logging"
	"github.com/ilyapiatykh/seine/internal/netpolicy"
	"github.com/ilyapiatykh/seine/internal/specsource"
	"github.com/ilyapiatykh/seine/internal/topology"
	"github.com/ilyapiatykh/seine/internal/wg"
)

// Mode picks between the two agent personalities. The thesis describes
// a multi-hub topology: hub binds a public UDP port and forwards traffic
// between spokes; spoke connects outbound only.
type Mode string

const (
	ModeSpoke Mode = "spoke"
	ModeHub   Mode = "hub"
)

// Config bundles the dependencies and tunables of an Agent.
type Config struct {
	// Identity declared in the network spec; must match exactly.
	Name string

	// Mode selects spoke vs hub behaviour.
	Mode Mode

	// Spec is the live view of the declarative network configuration.
	Spec *specsource.Watcher

	// ControlPlaneAddr is the host:port the agent dials for gRPC.
	ControlPlaneAddr string

	// BootstrapToken is the operator-issued shared secret used at the
	// first Register call. It can be empty on subsequent runs because
	// the auth token from the previous run is reused.
	BootstrapToken string

	// AdvertisedEndpoint is the host:port the agent reports to the
	// control plane so other peers can dial it. Required for hubs;
	// typically empty for spokes.
	AdvertisedEndpoint string

	// InterfaceName is the Linux netdev name (e.g. "seine0").
	InterfaceName string

	// StateDir holds persistent files: WireGuard private key and the
	// long-lived auth token. The agent expects 0700 permissions.
	StateDir string

	// ReconcileInterval governs the cadence of the main loop.
	ReconcileInterval time.Duration

	// SpecFirstLoadTimeout caps how long we wait for the first
	// successful Git pull before giving up.
	SpecFirstLoadTimeout time.Duration

	// RegisterRetryInterval delays between failed Register attempts.
	RegisterRetryInterval time.Duration
}

// applyDefaults fills sensible defaults for unset fields.
func (c *Config) applyDefaults() {
	if c.InterfaceName == "" {
		c.InterfaceName = "seine0"
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join("/var/lib/seine", c.Name)
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 30 * time.Second
	}
	if c.SpecFirstLoadTimeout <= 0 {
		c.SpecFirstLoadTimeout = 60 * time.Second
	}
	if c.RegisterRetryInterval <= 0 {
		c.RegisterRetryInterval = 5 * time.Second
	}
}

// Agent is the runtime state of a single seine-agent process.
type Agent struct {
	cfg Config

	keypair   wg.Keypair
	authToken string

	conn *grpc.ClientConn
	cp   cpv1.ControlPlaneClient

	iface    wg.Interface
	self     topology.Self
	firewall *netpolicy.Firewall // hub mode only

	keyPath   string
	tokenPath string

	// telemetry instruments; nil-safe through otel no-op providers.
	reconcileTotal    metric.Int64Counter
	reconcileDuration metric.Float64Histogram
}

// New constructs and validates an Agent.
func New(cfg Config) (*Agent, error) {
	if cfg.Name == "" {
		return nil, errors.New("agentcore: Name is required")
	}
	if cfg.Mode != ModeSpoke && cfg.Mode != ModeHub {
		return nil, fmt.Errorf("agentcore: invalid mode %q", cfg.Mode)
	}
	if cfg.Spec == nil {
		return nil, errors.New("agentcore: Spec is required")
	}
	if cfg.ControlPlaneAddr == "" {
		return nil, errors.New("agentcore: ControlPlaneAddr is required")
	}
	cfg.applyDefaults()

	a := &Agent{cfg: cfg}
	a.keyPath = filepath.Join(cfg.StateDir, "private.key")
	a.tokenPath = filepath.Join(cfg.StateDir, "auth.token")

	// OTel meter; resolves to a no-op provider when telemetry is off.
	meter := otel.Meter("github.com/ilyapiatykh/seine/agentcore")
	var err error
	a.reconcileTotal, err = meter.Int64Counter(
		"seine.reconcile.total",
		metric.WithDescription("Reconcile cycles by result."),
	)
	if err != nil {
		return nil, fmt.Errorf("agentcore: meter counter: %w", err)
	}
	a.reconcileDuration, err = meter.Float64Histogram(
		"seine.reconcile.duration",
		metric.WithDescription("Wall time of one reconcile cycle."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("agentcore: meter histogram: %w", err)
	}
	return a, nil
}

// Run executes the full agent lifecycle until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	log := logging.FromContext(ctx).With(
		slog.String("component", "agentcore"),
		slog.String("name", a.cfg.Name),
		slog.String("mode", string(a.cfg.Mode)),
	)
	ctx = logging.WithLogger(ctx, log)

	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("agentcore: state dir: %w", err)
	}

	kp, err := wg.LoadOrGenerate(a.keyPath)
	if err != nil {
		return err
	}
	a.keypair = kp
	log.Info("keypair loaded", slog.String("public_key", kp.Public.String()))

	conn, err := grpc.NewClient(a.cfg.ControlPlaneAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return fmt.Errorf("agentcore: dial control plane: %w", err)
	}
	defer conn.Close()
	a.conn = conn
	a.cp = cpv1.NewControlPlaneClient(conn)

	if err := a.loadOrRegister(ctx); err != nil {
		return err
	}

	if err := a.waitForFirstSpec(ctx); err != nil {
		return err
	}

	doc, _, err := a.cfg.Spec.Current()
	if err != nil {
		return fmt.Errorf("agentcore: spec unavailable: %w", err)
	}
	self, err := topology.FindSelf(doc, a.cfg.Name)
	if err != nil {
		return err
	}
	a.self = self
	if (self.Role == cpv1.Role_ROLE_HUB) != (a.cfg.Mode == ModeHub) {
		return fmt.Errorf("agentcore: --mode=%s but spec declares role %s", a.cfg.Mode, self.Role)
	}

	iface, err := wg.New(a.cfg.InterfaceName)
	if err != nil {
		return fmt.Errorf("agentcore: wg interface: %w", err)
	}
	a.iface = iface
	defer iface.Close()

	if err := a.bringUp(ctx); err != nil {
		return err
	}
	defer func() {
		downCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := iface.Down(downCtx); err != nil {
			log.Warn("interface teardown failed", slog.String("err", err.Error()))
		}
	}()

	if a.cfg.Mode == ModeHub {
		if err := netpolicy.EnsureIPForwarding(ctx); err != nil {
			return fmt.Errorf("agentcore: %w", err)
		}
		fw, err := netpolicy.NewFirewall(a.cfg.InterfaceName)
		if err != nil {
			return err
		}
		a.firewall = fw
		defer func() {
			downCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			fw.Teardown(downCtx)
		}()
	}

	log.Info("agent up",
		slog.String("role", self.Role.String()),
		slog.String("tunnel_ip", self.TunnelIP.String()),
		slog.String("interface", a.cfg.InterfaceName),
	)

	return a.reconcileLoop(ctx)
}

// loadOrRegister reuses a stored auth token if one is present; otherwise
// it registers with the control plane and persists the new token.
func (a *Agent) loadOrRegister(ctx context.Context) error {
	log := logging.FromContext(ctx)
	if data, err := os.ReadFile(a.tokenPath); err == nil {
		a.authToken = string(data)
		log.Info("reusing stored auth token")
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agentcore: read auth token: %w", err)
	}
	if a.cfg.BootstrapToken == "" {
		return errors.New("agentcore: no auth token on disk and --bootstrap-token is empty")
	}
	return a.registerWithRetry(ctx)
}

// registerWithRetry calls Register until it succeeds or ctx is cancelled.
// Permanent errors (PermissionDenied — name not in spec; Unauthenticated
// — bad bootstrap token) abort immediately because they cannot be fixed
// by retrying.
func (a *Agent) registerWithRetry(ctx context.Context) error {
	log := logging.FromContext(ctx)
	for attempt := 0; ; attempt++ {
		err := a.register(ctx)
		if err == nil {
			return nil
		}
		switch status.Code(err) {
		case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
			return err
		}
		log.Warn("register failed, will retry",
			slog.Int("attempt", attempt+1),
			slog.String("err", err.Error()),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.RegisterRetryInterval):
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	resp, err := a.cp.Register(ctx, &cpv1.RegisterRequest{
		Name:           a.cfg.Name,
		BootstrapToken: a.cfg.BootstrapToken,
		PublicKey:      a.keypair.Public.String(),
		Endpoint:       a.cfg.AdvertisedEndpoint,
		AgentVersion:   buildinfo.Short(),
	})
	if err != nil {
		return err
	}
	if resp.GetAuthToken() == "" {
		return errors.New("agentcore: server returned empty auth token")
	}
	a.authToken = resp.GetAuthToken()
	if err := os.WriteFile(a.tokenPath, []byte(a.authToken), 0o600); err != nil {
		return fmt.Errorf("agentcore: persist auth token: %w", err)
	}
	logging.FromContext(ctx).Info("registered with control plane")
	return nil
}

// waitForFirstSpec blocks until the spec watcher has produced a parseable
// document, with a configurable timeout to fail fast on misconfiguration.
func (a *Agent) waitForFirstSpec(ctx context.Context) error {
	if _, _, err := a.cfg.Spec.Current(); err == nil {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, a.cfg.SpecFirstLoadTimeout)
	defer cancel()
	for {
		select {
		case <-deadline.Done():
			return fmt.Errorf("agentcore: spec not loaded within %s", a.cfg.SpecFirstLoadTimeout)
		case <-a.cfg.Spec.Updates():
			if _, _, err := a.cfg.Spec.Current(); err == nil {
				return nil
			}
		}
	}
}

// bringUp configures and activates the local WireGuard interface based on
// the agent's role in the current spec.
func (a *Agent) bringUp(ctx context.Context) error {
	d, _, err := a.cfg.Spec.Current()
	if err != nil {
		return fmt.Errorf("agentcore: spec for bringUp: %w", err)
	}
	overlay, err := netip.ParsePrefix(d.Spec.CIDR)
	if err != nil {
		return fmt.Errorf("agentcore: parse overlay: %w", err)
	}
	address := netip.PrefixFrom(a.self.TunnelIP, overlay.Bits())

	listenPort := 0
	if a.cfg.Mode == ModeHub {
		// Prefer the port declared in the spec endpoint; fall back to
		// the network-wide hubListenPort.
		listenPort = d.Spec.WireGuard.HubListenPort
		if h := d.FindHub(a.cfg.Name); h != nil {
			if _, port, err := net.SplitHostPort(h.Endpoint); err == nil {
				if p, err := strconv.Atoi(port); err == nil {
					listenPort = p
				}
			}
		}
	}

	return a.iface.Up(ctx, wg.UpOptions{
		PrivateKey: a.keypair.Private,
		ListenPort: listenPort,
		Address:    address,
		MTU:        d.Spec.WireGuard.MTU,
	})
}

func (a *Agent) reconcileLoop(ctx context.Context) error {
	log := logging.FromContext(ctx)
	tick := time.NewTicker(a.cfg.ReconcileInterval)
	defer tick.Stop()

	// Reconcile immediately so we're not waiting a full tick on startup.
	if err := a.reconcileOnce(ctx); err != nil {
		log.Warn("initial reconcile failed", slog.String("err", err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received")
			return nil
		case <-tick.C:
		case <-a.cfg.Spec.Updates():
			log.Debug("spec change observed; reconciling early")
		}
		started := time.Now()
		err := a.reconcileOnce(ctx)
		dur := time.Since(started)
		result := "success"
		if err != nil {
			result = "failure"
			log.Warn("reconcile failed", slog.String("err", err.Error()))
		}
		a.reconcileTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
		a.reconcileDuration.Record(ctx, dur.Seconds())
	}
}

func (a *Agent) reconcileOnce(ctx context.Context) error {
	log := logging.FromContext(ctx)

	hbCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.authToken)
	resp, err := a.cp.Heartbeat(hbCtx, &cpv1.HeartbeatRequest{
		Endpoint: a.cfg.AdvertisedEndpoint,
	})
	if err != nil {
		// If the server forgot us (token rotation, DB wipe), re-register
		// transparently so the agent self-heals without operator action.
		if status.Code(err) == codes.Unauthenticated && a.cfg.BootstrapToken != "" {
			log.Warn("heartbeat unauthenticated; re-registering")
			if rerr := a.register(ctx); rerr != nil {
				return fmt.Errorf("re-register: %w", rerr)
			}
			return nil
		}
		return fmt.Errorf("heartbeat: %w", err)
	}

	doc, commit, err := a.cfg.Spec.Current()
	if err != nil {
		return fmt.Errorf("spec: %w", err)
	}

	if resp.GetSpecCommit() != "" && resp.GetSpecCommit() != commit {
		log.Debug("local spec lags server view",
			slog.String("local_commit", commit),
			slog.String("server_commit", resp.GetSpecCommit()),
		)
	}

	keepalive := doc.Spec.WireGuard.PersistentKeepalive.Std()
	desired, err := topology.PeersFor(doc, a.self, resp.GetPeers(), keepalive)
	if err != nil {
		return fmt.Errorf("compute peers: %w", err)
	}

	diff, err := a.iface.ApplyPeers(ctx, desired)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	if !diff.Empty() {
		log.Info("reconciled peers",
			slog.Int("desired", len(desired)),
			slog.Int("added", len(diff.Added)),
			slog.Int("updated", len(diff.Updated)),
			slog.Int("removed", len(diff.Removed)),
			slog.String("commit", commit),
		)
	} else {
		log.Debug("peers stable", slog.Int("count", len(desired)))
	}

	if a.firewall != nil {
		compiled, err := netpolicy.Compile(doc)
		if err != nil {
			return fmt.Errorf("compile acl: %w", err)
		}
		if err := a.firewall.Reconcile(ctx, compiled); err != nil {
			return fmt.Errorf("apply acl: %w", err)
		}
	}
	return nil
}
