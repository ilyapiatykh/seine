package gitsource_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/ilyapiatykh/seine/internal/gitsource"
)

// makeRepo creates a fresh git repo in a temp dir, writes file with content,
// commits it to master, and returns (file:// URL, working dir on disk).
func makeRepo(t *testing.T, file string, content []byte) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add(file); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sig := &object.Signature{Name: "test", Email: "t@example.com", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return "file://" + abs, dir
}

// updateRepo writes new content to file in the source repo (dir from makeRepo)
// and commits, returning the new HEAD SHA.
func updateRepo(t *testing.T, dir, file string, content []byte, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add(file); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sig := &object.Signature{Name: "test", Email: "t@example.com", When: time.Now()}
	hash, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return hash.String()
}

func TestSourcePull_CloneAndRead(t *testing.T) {
	url, _ := makeRepo(t, "network.yaml", []byte("hello: world\n"))

	src, err := gitsource.Open(gitsource.Config{
		URL:    url,
		Branch: "master",
		Path:   "network.yaml",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	snap, err := src.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if string(snap.Data) != "hello: world\n" {
		t.Errorf("data = %q", snap.Data)
	}
	if snap.CommitSHA == "" || len(snap.CommitSHA) != 40 {
		t.Errorf("commit sha looks wrong: %q", snap.CommitSHA)
	}
	if snap.Branch != "master" || snap.Path != "network.yaml" {
		t.Errorf("metadata = %+v", snap)
	}
}

func TestSourcePull_DetectsNewCommit(t *testing.T) {
	url, dir := makeRepo(t, "network.yaml", []byte("v: 1\n"))

	src, err := gitsource.Open(gitsource.Config{
		URL:    url,
		Branch: "master",
		Path:   "network.yaml",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	first, err := src.Pull(context.Background())
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}

	// Identical pull should be a no-op (NoErrAlreadyUpToDate path).
	again, err := src.Pull(context.Background())
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if again.CommitSHA != first.CommitSHA {
		t.Errorf("expected stable SHA across pulls: %s vs %s", first.CommitSHA, again.CommitSHA)
	}

	// Push a new commit upstream and verify the next pull observes it.
	updateRepo(t, dir, "network.yaml", []byte("v: 2\n"), "bump")
	third, err := src.Pull(context.Background())
	if err != nil {
		t.Fatalf("third Pull: %v", err)
	}
	if third.CommitSHA == first.CommitSHA {
		t.Errorf("expected new SHA after upstream commit, still %s", third.CommitSHA)
	}
	if string(third.Data) != "v: 2\n" {
		t.Errorf("expected updated content, got %q", third.Data)
	}
}

func TestSourcePull_MissingFile(t *testing.T) {
	url, _ := makeRepo(t, "network.yaml", []byte("ok\n"))

	src, err := gitsource.Open(gitsource.Config{
		URL:    url,
		Branch: "master",
		Path:   "does-not-exist.yaml",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	if _, err := src.Pull(context.Background()); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestSourcePull_MissingBranch(t *testing.T) {
	url, _ := makeRepo(t, "network.yaml", []byte("ok\n"))

	src, err := gitsource.Open(gitsource.Config{
		URL:    url,
		Branch: "ghost",
		Path:   "network.yaml",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	if _, err := src.Pull(context.Background()); err == nil {
		t.Fatal("expected error for missing branch")
	}
}

func TestOpenValidatesConfig(t *testing.T) {
	cases := []gitsource.Config{
		{Branch: "main", Path: "x"},
		{URL: "u", Path: "x"},
		{URL: "u", Branch: "main"},
	}
	for i, c := range cases {
		if _, err := gitsource.Open(c); err == nil {
			t.Errorf("case %d: expected error for %+v", i, c)
		}
	}
}
