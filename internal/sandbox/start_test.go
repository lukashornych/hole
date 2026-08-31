package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

func TestWorktreeMode(t *testing.T) {
	tests := map[string]worktree.LinkMode{
		"":         worktree.LinkReadOnly,
		"ro":       worktree.LinkReadOnly,
		"rw":       worktree.LinkReadWrite,
		"off":      worktree.LinkOff,
		"nonsense": worktree.LinkReadOnly,
	}
	for value, want := range tests {
		settings := &config.Settings{}
		settings.Git.WorktreeLinks = value
		if got := worktreeMode(settings); got != want {
			t.Errorf("worktreeLinks=%q -> %q, want %q", value, got, want)
		}
	}
}

// The pool has to exist before compose runs, and be created by the invoking user: a bind source
// the daemon creates is root-owned, and against a remote daemon a missing one silently resolves
// to an empty directory.
func TestEnsureWorktreePoolCreatesTheDirectory(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "myapp-worktrees")
	if err := ensureWorktreePool(pool); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pool)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("the pool is not a directory")
	}
	// Idempotent: a pool from an earlier run is reused, checkouts and all.
	if err := ensureWorktreePool(pool); err != nil {
		t.Errorf("an existing pool was rejected: %v", err)
	}
}

func TestEnsureWorktreePoolWithoutAPoolDoesNothing(t *testing.T) {
	if err := ensureWorktreePool(""); err != nil {
		t.Errorf("no pool requested, got %v", err)
	}
}

// Silently not getting the pool would leave the agent writing checkouts into a container-local
// directory that dies with the sandbox, so this fails the start.
func TestEnsureWorktreePoolRejectsANonDirectory(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "myapp-worktrees")
	if err := os.WriteFile(pool, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorktreePool(pool); err == nil {
		t.Error("a file in the pool's place was accepted")
	}
}
