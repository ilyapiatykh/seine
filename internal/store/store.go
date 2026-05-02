// Package store persists the management server's runtime state.
//
// The store is backed by SQLite (modernc.org/sqlite — pure Go, no cgo) and
// holds a single first-class entity: the Agent. It records each registered
// agent's identity (name, public key), most-recent advertised endpoint, the
// hash of its bearer token and the time of the last successful heartbeat.
//
// The declarative network spec is *not* persisted here: it lives in Git and
// is read into memory through a separate component.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Agent is a registered seine peer. Role is not persisted: it is recomputed
// from the spec on each request so that hub/spoke reassignment (a YAML edit)
// does not require operator intervention against the database.
type Agent struct {
	ID            string
	Name          string
	PublicKey     string
	Endpoint      string
	AuthTokenHash []byte
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

// Store is the SQLite-backed persistence layer.
type Store struct {
	db *sql.DB
}

// Open returns a Store at path. The schema is created on first use.
// path may be ":memory:" for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite likes a single writer; cap the pool to avoid lock churn.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// HashToken returns the storage form of an opaque bearer token. The server
// never stores the plain token: only this digest is persisted, and the
// agent keeps the plain token client-side.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// migrate creates tables on a fresh database. It is idempotent.
func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL UNIQUE,
	public_key      TEXT NOT NULL,
	endpoint        TEXT NOT NULL DEFAULT '',
	auth_token_hash BLOB NOT NULL,
	created_at      INTEGER NOT NULL,
	last_seen_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_agents_token ON agents(auth_token_hash);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// UpsertAgent registers or re-registers an agent by name. The caller is
// responsible for generating id and authTokenHash on first insert; on
// re-registration both are replaced (the original token is invalidated).
func (s *Store) UpsertAgent(ctx context.Context, a Agent) error {
	now := time.Now().UTC().Unix()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Unix(now, 0).UTC()
	}
	const q = `
INSERT INTO agents (id, name, public_key, endpoint, auth_token_hash, created_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
	id              = excluded.id,
	public_key      = excluded.public_key,
	endpoint        = excluded.endpoint,
	auth_token_hash = excluded.auth_token_hash,
	last_seen_at    = excluded.last_seen_at
`
	_, err := s.db.ExecContext(ctx, q,
		a.ID, a.Name, a.PublicKey, a.Endpoint, a.AuthTokenHash,
		a.CreatedAt.UTC().Unix(), a.LastSeenAt.UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: upsert agent: %w", err)
	}
	return nil
}

// GetAgentByName returns the agent with the given name.
func (s *Store) GetAgentByName(ctx context.Context, name string) (*Agent, error) {
	const q = `SELECT id, name, public_key, endpoint, auth_token_hash, created_at, last_seen_at FROM agents WHERE name = ?`
	row := s.db.QueryRowContext(ctx, q, name)
	return scanAgent(row)
}

// AuthenticateByTokenHash looks up the agent whose token digest matches.
// Returns ErrNotFound if no such agent exists.
func (s *Store) AuthenticateByTokenHash(ctx context.Context, hash []byte) (*Agent, error) {
	const q = `SELECT id, name, public_key, endpoint, auth_token_hash, created_at, last_seen_at FROM agents WHERE auth_token_hash = ?`
	row := s.db.QueryRowContext(ctx, q, hash)
	return scanAgent(row)
}

// UpdateHeartbeat records a successful heartbeat. If endpoint is empty the
// previously stored endpoint is preserved.
func (s *Store) UpdateHeartbeat(ctx context.Context, id, endpoint string, when time.Time) error {
	if endpoint == "" {
		const q = `UPDATE agents SET last_seen_at = ? WHERE id = ?`
		_, err := s.db.ExecContext(ctx, q, when.UTC().Unix(), id)
		return err
	}
	const q = `UPDATE agents SET endpoint = ?, last_seen_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q, endpoint, when.UTC().Unix(), id)
	return err
}

// ListAgents returns all registered agents ordered by name.
func (s *Store) ListAgents(ctx context.Context) ([]*Agent, error) {
	const q = `SELECT id, name, public_key, endpoint, auth_token_hash, created_at, last_seen_at FROM agents ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanner abstracts *sql.Row and *sql.Rows for scanAgent.
type scanner interface {
	Scan(dest ...any) error
}

func scanAgent(s scanner) (*Agent, error) {
	var a Agent
	var created, seen int64
	err := s.Scan(&a.ID, &a.Name, &a.PublicKey, &a.Endpoint, &a.AuthTokenHash, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	if seen > 0 {
		a.LastSeenAt = time.Unix(seen, 0).UTC()
	}
	return &a, nil
}
