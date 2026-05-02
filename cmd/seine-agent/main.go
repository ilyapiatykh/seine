// Command seine-agent is the data-plane component of a seine deployment.
//
// A single binary runs in either spoke or hub mode (selected via --mode).
// On startup it generates and persists a WireGuard keypair, registers
// with the management server using a bootstrap token, brings up the
// local WireGuard interface and runs a reconciliation loop that pulls
// the network spec from Git and applies the matching peer set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ilyapiatykh/seine/internal/agentcore"
	"github.com/ilyapiatykh/seine/internal/buildinfo"
	"github.com/ilyapiatykh/seine/internal/gitsource"
	"github.com/ilyapiatykh/seine/internal/logging"
	otelsetup "github.com/ilyapiatykh/seine/internal/otel"
	"github.com/ilyapiatykh/seine/internal/specsource"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seine-agent:", err)
		os.Exit(1)
	}
}

type options struct {
	showVersion bool

	logLevel  string
	logFormat string

	name               string
	mode               string
	controlPlaneAddr   string
	bootstrapToken     string
	advertisedEndpoint string

	interfaceName string
	stateDir      string

	reconcileInterval time.Duration

	gitURL      string
	gitBranch   string
	gitPath     string
	gitWorkdir  string
	gitInterval time.Duration
	gitToken    string
	gitUsername string
	gitSSHKey   string

	otlpEndpoint string
	otlpInsecure bool
}

func parseFlags() *options {
	var o options
	flag.BoolVar(&o.showVersion, "version", false, "print version and exit")

	flag.StringVar(&o.logLevel, "log-level", envOr("SEINE_LOG_LEVEL", "info"),
		"log level: debug, info, warn, error")
	flag.StringVar(&o.logFormat, "log-format", envOr("SEINE_LOG_FORMAT", "text"),
		"log format: text, json")

	flag.StringVar(&o.name, "name", os.Getenv("SEINE_AGENT_NAME"),
		"agent name as declared in the network spec")
	flag.StringVar(&o.mode, "mode", envOr("SEINE_MODE", "spoke"),
		"agent mode: spoke or hub")
	flag.StringVar(&o.controlPlaneAddr, "control-plane", os.Getenv("SEINE_CONTROL_PLANE"),
		"host:port of the management server gRPC endpoint")
	flag.StringVar(&o.bootstrapToken, "bootstrap-token", os.Getenv("SEINE_BOOTSTRAP_TOKEN"),
		"shared secret used at first registration (not needed afterwards)")
	flag.StringVar(&o.advertisedEndpoint, "advertise-endpoint",
		os.Getenv("SEINE_ADVERTISE_ENDPOINT"),
		"public host:port other peers should dial (required for hubs)")

	flag.StringVar(&o.interfaceName, "interface", envOr("SEINE_INTERFACE", "seine0"),
		"WireGuard interface name")
	flag.StringVar(&o.stateDir, "state-dir", os.Getenv("SEINE_STATE_DIR"),
		"directory for the WG private key and auth token (defaults to /var/lib/seine/<name>)")

	flag.DurationVar(&o.reconcileInterval, "reconcile-interval",
		durationEnv("SEINE_RECONCILE_INTERVAL", 30*time.Second),
		"how often the agent reconciles peers")

	flag.StringVar(&o.gitURL, "git-url", os.Getenv("SEINE_GIT_URL"),
		"URL of the Git repository holding the spec")
	flag.StringVar(&o.gitBranch, "git-branch", envOr("SEINE_GIT_BRANCH", "main"),
		"branch to track")
	flag.StringVar(&o.gitPath, "git-path", envOr("SEINE_GIT_PATH", "network.yaml"),
		"path to the spec file inside the repository")
	flag.StringVar(&o.gitWorkdir, "git-workdir", os.Getenv("SEINE_GIT_WORKDIR"),
		"local directory for the Git working copy (empty: tempdir)")
	flag.DurationVar(&o.gitInterval, "git-interval",
		durationEnv("SEINE_GIT_INTERVAL", 30*time.Second),
		"interval between Git pulls")
	flag.StringVar(&o.gitToken, "git-token", os.Getenv("SEINE_GIT_TOKEN"),
		"HTTPS token for the Git remote (optional)")
	flag.StringVar(&o.gitUsername, "git-username", envOr("SEINE_GIT_USERNAME", "git"),
		"HTTPS username paired with --git-token")
	flag.StringVar(&o.gitSSHKey, "git-ssh-key", os.Getenv("SEINE_GIT_SSH_KEY"),
		"path to an OpenSSH private key (alternative to --git-token)")

	flag.StringVar(&o.otlpEndpoint, "otlp-endpoint", os.Getenv("SEINE_OTLP_ENDPOINT"),
		"OTLP/gRPC collector endpoint host:port (empty disables export)")
	flag.BoolVar(&o.otlpInsecure, "otlp-insecure",
		envOr("SEINE_OTLP_INSECURE", "true") == "true",
		"send OTLP without TLS (demo default)")

	flag.Parse()
	return &o
}

func run() error {
	opts := parseFlags()
	if opts.showVersion {
		fmt.Println("seine-agent", buildinfo.Short())
		return nil
	}

	level, err := logging.ParseLevel(opts.logLevel)
	if err != nil {
		return err
	}
	format, err := logging.ParseFormat(opts.logFormat)
	if err != nil {
		return err
	}
	log := logging.Setup(logging.Options{Level: level, Format: format})
	log.Info("starting seine-agent",
		slog.String("version", buildinfo.Short()),
		slog.String("name", opts.name),
		slog.String("mode", opts.mode),
	)

	if opts.name == "" {
		return errors.New("--name (or SEINE_AGENT_NAME) is required")
	}
	if opts.controlPlaneAddr == "" {
		return errors.New("--control-plane (or SEINE_CONTROL_PLANE) is required")
	}
	if opts.gitURL == "" {
		return errors.New("--git-url (or SEINE_GIT_URL) is required")
	}
	mode := agentcore.Mode(opts.mode)
	if mode != agentcore.ModeSpoke && mode != agentcore.ModeHub {
		return fmt.Errorf("invalid --mode %q (expected spoke or hub)", opts.mode)
	}
	if mode == agentcore.ModeHub && opts.advertisedEndpoint == "" {
		return errors.New("--advertise-endpoint is required for hubs")
	}

	stateDir := opts.stateDir
	if stateDir == "" {
		stateDir = filepath.Join("/var/lib/seine", opts.name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = logging.WithLogger(ctx, log)

	otelShutdown, err := otelsetup.Setup(ctx, otelsetup.Config{
		ServiceName:    "seine-agent",
		ServiceVersion: buildinfo.Version,
		OTLPEndpoint:   opts.otlpEndpoint,
		Insecure:       opts.otlpInsecure,
	})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			log.Warn("otel shutdown", slog.String("err", err.Error()))
		}
	}()

	auth, err := buildGitAuth(opts)
	if err != nil {
		return err
	}
	watcher, err := specsource.New(specsource.Config{
		Source: gitsource.Config{
			URL:     opts.gitURL,
			Branch:  opts.gitBranch,
			Path:    opts.gitPath,
			Workdir: opts.gitWorkdir,
			Auth:    auth,
		},
		Interval: opts.gitInterval,
	})
	if err != nil {
		return fmt.Errorf("specsource: %w", err)
	}
	defer watcher.Close()

	go func() {
		if err := watcher.Run(ctx); err != nil {
			log.Warn("specsource watcher exited", slog.String("err", err.Error()))
		}
	}()

	agent, err := agentcore.New(agentcore.Config{
		Name:               opts.name,
		Mode:               mode,
		Spec:               watcher,
		ControlPlaneAddr:   opts.controlPlaneAddr,
		BootstrapToken:     opts.bootstrapToken,
		AdvertisedEndpoint: opts.advertisedEndpoint,
		InterfaceName:      opts.interfaceName,
		StateDir:           stateDir,
		ReconcileInterval:  opts.reconcileInterval,
	})
	if err != nil {
		return err
	}

	if err := agent.Run(ctx); err != nil {
		return err
	}
	log.Info("seine-agent stopped")
	return nil
}

func buildGitAuth(o *options) (gitsource.Auth, error) {
	switch {
	case o.gitToken != "":
		return gitsource.TokenAuth{Username: o.gitUsername, Token: o.gitToken}, nil
	case o.gitSSHKey != "":
		return gitsource.SSHKeyAuth{User: "git", PrivateKeyPath: o.gitSSHKey}, nil
	default:
		return gitsource.NoAuth{}, nil
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
