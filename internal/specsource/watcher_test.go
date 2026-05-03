package specsource_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/ilyapiatykh/seine/internal/gitsource"
	"github.com/ilyapiatykh/seine/internal/specsource"
)

const exampleSpec = `apiVersion: seine.io/v1
kind: Network
metadata:
  name: corp
spec:
  cidr: 100.64.0.0/10
  hubs:
    - name: hub1
      endpoint: hub.example.com:51820
      tunnelIP: 100.64.0.1
  agents:
    - name: spoke1
      tunnelIP: 100.64.1.1
      hub: hub1
      groups: [g]
  groups: [g]
  acls:
    - from: [g]
      to: [g]
      action: allow
`

const exampleSpec2 = `apiVersion: seine.io/v1
kind: Network
metadata:
  name: corp
spec:
  cidr: 100.64.0.0/10
  hubs:
    - name: hub1
      endpoint: hub.example.com:51820
      tunnelIP: 100.64.0.1
    - name: hub2
      endpoint: hub2.example.com:51820
      tunnelIP: 100.64.0.2
  agents:
    - name: spoke1
      tunnelIP: 100.64.1.1
      hub: hub1
      groups: [g]
  groups: [g]
  acls:
    - from: [g]
      to: [g]
      action: allow
`

func makeRepo(t *testing.T, body string) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("network.yaml"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sig := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	return "file://" + abs, dir
}

func updateRepo(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "network.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repo, _ := git.PlainOpen(dir)
	wt, _ := repo.Worktree()
	if _, err := wt.Add("network.yaml"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sig := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}
	if _, err := wt.Commit("update", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestWatcher_RunLoadsAndUpdates(t *testing.T) {
	url, dir := makeRepo(t, exampleSpec)
	w, err := specsource.New(specsource.Config{
		Source:     gitsource.Config{URL: url, Branch: "master", Path: "network.yaml"},
		Interval:   100 * time.Millisecond,
		MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// Wait for the first successful pull.
	select {
	case <-w.Updates():
	case <-time.After(5 * time.Second):
		t.Fatal("first update not delivered within 5s")
	}

	doc, sha1, err := w.Current()
	if err != nil {
		t.Fatalf("Current after first pull: %v", err)
	}
	if len(doc.Spec.Hubs) != 1 {
		t.Fatalf("expected 1 hub, got %d", len(doc.Spec.Hubs))
	}
	if sha1 == "" {
		t.Fatal("commit sha is empty")
	}

	// Push a new commit upstream and wait for the watcher to observe it.
	updateRepo(t, dir, exampleSpec2)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("watcher did not pick up the new commit")
		case <-w.Updates():
		}
		doc, sha2, err := w.Current()
		if err != nil {
			t.Fatalf("Current after update: %v", err)
		}
		if sha2 != sha1 && len(doc.Spec.Hubs) == 2 {
			return // happy path
		}
	}
}

func TestWatcher_CurrentReportsErrorBeforeFirstPull(t *testing.T) {
	w, err := specsource.New(specsource.Config{
		Source: gitsource.Config{URL: "file:///nonexistent", Branch: "main", Path: "x"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, _, err := w.Current(); err == nil {
		t.Fatal("expected error before first pull")
	}
}
