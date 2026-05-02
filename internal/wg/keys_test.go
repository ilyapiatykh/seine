package wg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ilyapiatykh/seine/internal/wg"
)

func TestGenerateKeypairUnique(t *testing.T) {
	a, err := wg.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	b, err := wg.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if a.Private == b.Private {
		t.Fatal("two generated keypairs are equal — should be vanishingly improbable")
	}
	if a.Public != a.Private.PublicKey() {
		t.Errorf("public key derivation mismatch")
	}
}

func TestLoadOrGenerate_GeneratesOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.key")

	first, err := wg.LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600 perms, got %o", info.Mode().Perm())
	}

	second, err := wg.LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if second.Private != first.Private {
		t.Errorf("LoadOrGenerate returned different keys for the same path")
	}
}

func TestLoadOrGenerate_RejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(path, []byte("this-is-not-a-key"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wg.LoadOrGenerate(path); err == nil {
		t.Fatal("expected error for corrupt key, got nil")
	}
}
