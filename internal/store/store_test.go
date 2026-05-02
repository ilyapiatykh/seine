package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ilyapiatykh/seine/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	a := store.Agent{
		ID:            "uuid-1",
		Name:          "hub-eu",
		PublicKey:     "pubkey1",
		Endpoint:      "1.2.3.4:51820",
		AuthTokenHash: store.HashToken("plain-token-1"),
		CreatedAt:     now,
		LastSeenAt:    now,
	}
	if err := s.UpsertAgent(ctx, a); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	got, err := s.GetAgentByName(ctx, "hub-eu")
	if err != nil {
		t.Fatalf("GetAgentByName: %v", err)
	}
	if got.ID != a.ID || got.PublicKey != a.PublicKey || got.Endpoint != a.Endpoint {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
}

func TestUpsertReplacesByName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := store.Agent{
		ID: "uuid-1", Name: "agent",
		PublicKey: "k1", Endpoint: "1.1.1.1:1",
		AuthTokenHash: store.HashToken("t1"),
		CreatedAt:     time.Now(), LastSeenAt: time.Now(),
	}
	if err := s.UpsertAgent(ctx, first); err != nil {
		t.Fatalf("UpsertAgent first: %v", err)
	}

	second := first
	second.ID = "uuid-2"
	second.PublicKey = "k2"
	second.AuthTokenHash = store.HashToken("t2")
	if err := s.UpsertAgent(ctx, second); err != nil {
		t.Fatalf("UpsertAgent second: %v", err)
	}

	got, err := s.GetAgentByName(ctx, "agent")
	if err != nil {
		t.Fatalf("GetAgentByName: %v", err)
	}
	if got.ID != "uuid-2" || got.PublicKey != "k2" {
		t.Errorf("re-register did not replace: %+v", got)
	}
	// Old token should not authenticate any more.
	if _, err := s.AuthenticateByTokenHash(ctx, store.HashToken("t1")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token still valid: err=%v", err)
	}
}

func TestAuthenticateByTokenHash(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := store.Agent{
		ID: "id", Name: "n", PublicKey: "k", Endpoint: "",
		AuthTokenHash: store.HashToken("secret-token"),
		CreatedAt:     time.Now(),
	}
	if err := s.UpsertAgent(ctx, a); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	got, err := s.AuthenticateByTokenHash(ctx, store.HashToken("secret-token"))
	if err != nil {
		t.Fatalf("AuthenticateByTokenHash: %v", err)
	}
	if got.Name != "n" {
		t.Errorf("got name %q, want n", got.Name)
	}
	if _, err := s.AuthenticateByTokenHash(ctx, store.HashToken("wrong")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for wrong token, got %v", err)
	}
}

func TestUpdateHeartbeat(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := store.Agent{
		ID: "id", Name: "n",
		PublicKey: "k", Endpoint: "1.1.1.1:1",
		AuthTokenHash: store.HashToken("t"),
		CreatedAt:     time.Now(),
	}
	if err := s.UpsertAgent(ctx, a); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	t1 := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := s.UpdateHeartbeat(ctx, a.ID, "2.2.2.2:2", t1); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	got, _ := s.GetAgentByName(ctx, "n")
	if got.Endpoint != "2.2.2.2:2" {
		t.Errorf("endpoint not updated: %q", got.Endpoint)
	}
	if !got.LastSeenAt.Equal(t1) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, t1)
	}

	// Empty endpoint should preserve previous value.
	t2 := t1.Add(time.Minute)
	if err := s.UpdateHeartbeat(ctx, a.ID, "", t2); err != nil {
		t.Fatalf("UpdateHeartbeat empty: %v", err)
	}
	got, _ = s.GetAgentByName(ctx, "n")
	if got.Endpoint != "2.2.2.2:2" {
		t.Errorf("empty endpoint should preserve, got %q", got.Endpoint)
	}
	if !got.LastSeenAt.Equal(t2) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, t2)
	}
}

func TestListAgents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, n := range []string{"b", "a", "c"} {
		_ = s.UpsertAgent(ctx, store.Agent{
			ID: n + "-id", Name: n,
			PublicKey: "k", AuthTokenHash: store.HashToken(n),
			CreatedAt: time.Now(),
		})
	}
	all, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	if all[0].Name != "a" || all[1].Name != "b" || all[2].Name != "c" {
		t.Errorf("not ordered: %+v", []string{all[0].Name, all[1].Name, all[2].Name})
	}
}
