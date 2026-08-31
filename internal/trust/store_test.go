package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	holeDir := t.TempDir()
	store := NewStore(holeDir)

	if store.Trusted("/projects/repo", "digest-a") {
		t.Error("an empty store trusted a project")
	}
	if err := store.Trust("/projects/repo", "digest-a", []string{"hooks.setupHost"}); err != nil {
		t.Fatal(err)
	}
	if !store.Trusted("/projects/repo", "digest-a") {
		t.Error("a recorded decision was not honored")
	}
	if store.Trusted("/projects/repo", "digest-b") {
		t.Error("a project was trusted for a grant set it never accepted")
	}
	if store.Trusted("/projects/other", "digest-a") {
		t.Error("a decision leaked to another project")
	}

	// A second project must not replace the first, and a re-grant must replace only its own.
	if err := store.Trust("/projects/other", "digest-b", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Trust("/projects/repo", "digest-c", nil); err != nil {
		t.Fatal(err)
	}
	if !store.Trusted("/projects/other", "digest-b") || !store.Trusted("/projects/repo", "digest-c") {
		t.Error("recording one decision disturbed another")
	}
	if store.Trusted("/projects/repo", "digest-a") {
		t.Error("a replaced decision is still trusted")
	}

	// Decisions survive a restart, and the file is the user's alone to read.
	if !NewStore(holeDir).Trusted("/projects/repo", "digest-c") {
		t.Error("a decision did not survive reopening the store")
	}
	info, err := os.Stat(filepath.Join(holeDir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("trust file mode = %v, want 0600", mode)
	}
}

// A record Hole cannot read must never be read as a grant: the failure direction has to be
// another prompt.
func TestStoreWithAnUnreadableRecord(t *testing.T) {
	holeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(holeDir, fileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(holeDir)
	if store.Trusted("/projects/repo", "digest-a") {
		t.Error("a corrupt trust file granted trust")
	}
	if err := store.Trust("/projects/repo", "digest-a", nil); err == nil {
		t.Error("Trust silently overwrote a file it could not parse")
	}
}
