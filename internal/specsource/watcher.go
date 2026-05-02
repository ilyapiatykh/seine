// Package specsource glues gitsource and spec into a single component that
// "pulls a YAML file from a Git remote, parses+validates it, and caches the
// most recent successful version".
//
// Both seine-server and seine-agent embed a Watcher: it satisfies the
// SpecProvider interface used by the control plane and is also what the
// agent's reconciliation loop consults each iteration.
package specsource

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ilyapiatykh/seine/internal/gitsource"
	"github.com/ilyapiatykh/seine/internal/logging"
	"github.com/ilyapiatykh/seine/internal/spec"
)

// Config configures a Watcher.
type Config struct {
	// Source defines the Git remote to track.
	Source gitsource.Config

	// Interval between successful poll cycles. Defaults to 30s if zero.
	Interval time.Duration

	// MinBackoff is the initial delay used when a pull fails. Doubles up
	// to MaxBackoff. Defaults: 2s and 60s.
	MinBackoff, MaxBackoff time.Duration
}

// Watcher periodically pulls a Git source and exposes the latest parsed
// network spec.
type Watcher struct {
	cfg Config
	src *gitsource.Source

	mu    sync.RWMutex
	state *snapshot
	last  error // last pull/parse error, if state is nil

	updates chan struct{}
}

type snapshot struct {
	doc    *spec.Document
	commit string
	at     time.Time
}

// New constructs a Watcher and opens the Git source. It does not contact
// the remote — Run does that.
func New(cfg Config) (*Watcher, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	src, err := gitsource.Open(cfg.Source)
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg:     cfg,
		src:     src,
		last:    errors.New("specsource: not yet loaded"),
		updates: make(chan struct{}, 1),
	}, nil
}

// Close releases the underlying Git source.
func (w *Watcher) Close() error { return w.src.Close() }

// Current returns the most recent successfully parsed spec along with its
// commit SHA. Until at least one pull succeeds it returns an error.
func (w *Watcher) Current() (*spec.Document, string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.state != nil {
		return w.state.doc, w.state.commit, nil
	}
	return nil, "", w.last
}

// Updates returns a channel that receives a value whenever the cached
// commit SHA changes. The channel is single-slot and lossy: receivers see
// "something changed since you last looked", not every event.
func (w *Watcher) Updates() <-chan struct{} { return w.updates }

// Run polls the remote on Cfg.Interval until ctx is cancelled. Transient
// failures are logged and retried with exponential backoff; once a pull
// succeeds the next attempt is scheduled at the configured interval.
func (w *Watcher) Run(ctx context.Context) error {
	log := logging.FromContext(ctx).With(slog.String("component", "specsource"))

	backoff := w.cfg.MinBackoff
	for {
		err := w.pullOnce(ctx)
		var delay time.Duration
		if err == nil {
			backoff = w.cfg.MinBackoff
			delay = w.cfg.Interval
		} else {
			log.Warn("specsource pull failed", slog.String("err", err.Error()),
				slog.Duration("retry_in", backoff))
			delay = backoff
			backoff *= 2
			if backoff > w.cfg.MaxBackoff {
				backoff = w.cfg.MaxBackoff
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// pullOnce performs a single Git pull and parses the result. It updates
// the cached state on success and surfaces last-error on failure.
func (w *Watcher) pullOnce(ctx context.Context) error {
	log := logging.FromContext(ctx).With(slog.String("component", "specsource"))

	snap, err := w.src.Pull(ctx)
	if err != nil {
		w.recordError(err)
		return err
	}
	doc, err := spec.Parse(snap.Data)
	if err != nil {
		w.recordError(err)
		return err
	}

	w.mu.Lock()
	prev := ""
	if w.state != nil {
		prev = w.state.commit
	}
	w.state = &snapshot{doc: doc, commit: snap.CommitSHA, at: time.Now().UTC()}
	w.last = nil
	w.mu.Unlock()

	if prev != snap.CommitSHA {
		log.Info("spec updated",
			slog.String("commit", snap.CommitSHA),
			slog.String("from", prev),
		)
		select {
		case w.updates <- struct{}{}:
		default:
		}
	}
	return nil
}

func (w *Watcher) recordError(err error) {
	w.mu.Lock()
	w.last = err
	w.mu.Unlock()
}
