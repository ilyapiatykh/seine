// Command seine-server is the management plane for a seine VPN deployment.
//
// It exposes a gRPC API for agents to register and synchronise runtime
// state, and pulls the declarative network specification from a Git
// repository. Authentication is bearer-token based; agents register once
// using a shared bootstrap token and receive a per-agent long-lived token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/ilyapiatykh/seine/internal/buildinfo"
	"github.com/ilyapiatykh/seine/internal/controlplane"
	"github.com/ilyapiatykh/seine/internal/gitsource"
	"github.com/ilyapiatykh/seine/internal/logging"
	otelsetup "github.com/ilyapiatykh/seine/internal/otel"
	"github.com/ilyapiatykh/seine/internal/specsource"
	"github.com/ilyapiatykh/seine/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seine-server:", err)
		os.Exit(1)
	}
}

type options struct {
	showVersion bool

	logLevel  string
	logFormat string

	listenAddr     string
	dbPath         string
	bootstrapToken string

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

func parseFlags() (*options, error) {
	var o options
	flag.BoolVar(&o.showVersion, "version", false, "print version and exit")

	flag.StringVar(&o.logLevel, "log-level", envOr("SEINE_LOG_LEVEL", "info"),
		"log level: debug, info, warn, error")
	flag.StringVar(&o.logFormat, "log-format", envOr("SEINE_LOG_FORMAT", "text"),
		"log format: text, json")

	flag.StringVar(&o.listenAddr, "listen", envOr("SEINE_LISTEN", ":8443"),
		"gRPC listen address (host:port)")
	flag.StringVar(&o.dbPath, "db", envOr("SEINE_DB", "seine.db"),
		"path to the SQLite database file")
	flag.StringVar(&o.bootstrapToken, "bootstrap-token", os.Getenv("SEINE_BOOTSTRAP_TOKEN"),
		"shared secret agents present at Register time")

	flag.StringVar(&o.gitURL, "git-url", os.Getenv("SEINE_GIT_URL"),
		"URL of the Git repository holding the spec")
	flag.StringVar(&o.gitBranch, "git-branch", envOr("SEINE_GIT_BRANCH", "main"),
		"branch to track")
	flag.StringVar(&o.gitPath, "git-path", envOr("SEINE_GIT_PATH", "network.yaml"),
		"path to the spec file inside the repository")
	flag.StringVar(&o.gitWorkdir, "git-workdir", envOr("SEINE_GIT_WORKDIR", ""),
		"local directory for the Git working copy (empty: tempdir)")
	flag.DurationVar(&o.gitInterval, "git-interval", durationEnv("SEINE_GIT_INTERVAL", 30*time.Second),
		"interval between Git pulls")
	flag.StringVar(&o.gitToken, "git-token", os.Getenv("SEINE_GIT_TOKEN"),
		"HTTPS token for the Git remote (optional)")
	flag.StringVar(&o.gitUsername, "git-username", envOr("SEINE_GIT_USERNAME", "git"),
		"HTTPS username paired with --git-token (defaults to 'git')")
	flag.StringVar(&o.gitSSHKey, "git-ssh-key", os.Getenv("SEINE_GIT_SSH_KEY"),
		"path to an OpenSSH private key (alternative to --git-token)")

	flag.StringVar(&o.otlpEndpoint, "otlp-endpoint", os.Getenv("SEINE_OTLP_ENDPOINT"),
		"OTLP/gRPC collector endpoint host:port (empty disables export)")
	flag.BoolVar(&o.otlpInsecure, "otlp-insecure",
		envOr("SEINE_OTLP_INSECURE", "true") == "true",
		"send OTLP without TLS (demo default)")

	flag.Parse()
	return &o, nil
}

func run() error {
	opts, err := parseFlags()
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Println("seine-server", buildinfo.Short())
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
	log.Info("starting seine-server",
		slog.String("version", buildinfo.Short()),
		slog.String("listen", opts.listenAddr),
		slog.String("db", opts.dbPath),
		slog.String("git_url", opts.gitURL),
		slog.String("git_branch", opts.gitBranch),
	)

	if opts.bootstrapToken == "" {
		return errors.New("--bootstrap-token (or SEINE_BOOTSTRAP_TOKEN) is required")
	}
	if opts.gitURL == "" {
		return errors.New("--git-url (or SEINE_GIT_URL) is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = logging.WithLogger(ctx, log)

	otelShutdown, err := otelsetup.Setup(ctx, otelsetup.Config{
		ServiceName:    "seine-server",
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

	st, err := store.Open(ctx, opts.dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

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

	cps, err := controlplane.NewServer(controlplane.Config{
		Store:          st,
		Spec:           watcher,
		BootstrapToken: opts.bootstrapToken,
	})
	if err != nil {
		return err
	}

	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(controlplane.AuthInterceptor(st, controlplane.SkipAuthMethods())),
	)
	cps.AttachTo(srv)

	lis, err := net.Listen("tcp", opts.listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Run the spec watcher concurrently with the gRPC server. Cancellation
	// flows top-down: a SIGTERM cancels ctx, which triggers GracefulStop
	// below; the watcher's Run also observes ctx.Done.
	go func() {
		if err := watcher.Run(ctx); err != nil {
			log.Warn("specsource watcher exited", slog.String("err", err.Error()))
		}
	}()
	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received, stopping gRPC server")
		srv.GracefulStop()
	}()

	log.Info("gRPC server ready", slog.String("addr", lis.Addr().String()))
	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("grpc serve: %w", err)
	}
	log.Info("seine-server stopped")
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
