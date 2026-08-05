package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// tempDir returns a temporary directory with symlinks resolved, which is the spelling git
// reports: on macOS every t.TempDir() lives under the /var -> /private/var symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// repo creates a git repository with one commit, or skips when git is unavailable.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := tempDir(t)
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestDeriveNothingOutsideAGitRepository(t *testing.T) {
	if links := Derive(t.TempDir(), LinkReadOnly); links != nil {
		t.Errorf("a plain directory yielded %v", links)
	}
}

func TestDeriveNothingWhenOff(t *testing.T) {
	if links := Derive(repo(t), LinkOff); links != nil {
		t.Errorf("worktreeLinks=off yielded %v", links)
	}
}

func TestDeriveNothingForAPlainRepositoryWithoutWorktrees(t *testing.T) {
	if links := Derive(repo(t), LinkReadOnly); len(links) != 0 {
		t.Errorf("a repository with no linked worktrees yielded %v", links)
	}
}

func TestDeriveLinksSiblingWorktreeFromTheMainRepository(t *testing.T) {
	main := repo(t)
	sibling := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", sibling)

	links := Derive(main, LinkReadOnly)
	if len(links) != 1 {
		t.Fatalf("expected the sibling worktree, got %v", links)
	}
	if !sameDir(links[0].HostPath, sibling) {
		t.Errorf("linked %s, want %s", links[0].HostPath, sibling)
	}
	if links[0].ReadWrite {
		t.Error("worktree links must be read-only by default")
	}
}

func TestDeriveLinksMainRepositoryFromALinkedWorktree(t *testing.T) {
	main := repo(t)
	linked := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	// A linked worktree's .git is only a pointer, so without the main repository the agent
	// cannot run git at all.
	links := Derive(linked, LinkReadOnly)
	if len(links) != 1 {
		t.Fatalf("expected the main repository, got %v", links)
	}
	if !sameDir(links[0].HostPath, main) {
		t.Errorf("linked %s, want %s", links[0].HostPath, main)
	}
}

func TestDeriveReadWriteMode(t *testing.T) {
	main := repo(t)
	sibling := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", sibling)

	links := Derive(main, LinkReadWrite)
	if len(links) != 1 || !links[0].ReadWrite {
		t.Errorf("links = %+v, want one read-write link", links)
	}
}

func TestDeriveSkipsWorktreesInsideTheProject(t *testing.T) {
	main := repo(t)
	// A worktree below the project directory is already covered by the project mount.
	inside := filepath.Join(main, "nested", "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", inside)

	if links := Derive(main, LinkReadOnly); len(links) != 0 {
		t.Errorf("a worktree inside the project yielded %v", links)
	}
}

// A project reached through a symlink must behave exactly like the physical path. git resolves
// symlinks in everything it prints, so comparing its output against the project directory as
// given used to make a plain repository look like a linked worktree of itself — mounting the
// project a second time, read-only, at its physical path.
func TestDeriveThroughASymlinkedProjectDirectory(t *testing.T) {
	main := repo(t)
	link := filepath.Join(tempDir(t), "alias")
	if err := os.Symlink(main, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if links := Derive(link, LinkReadOnly); len(links) != 0 {
		t.Errorf("a symlinked plain repository yielded %v", links)
	}

	sibling := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", sibling)

	links := Derive(link, LinkReadOnly)
	if len(links) != 1 {
		t.Fatalf("expected only the sibling worktree, got %v", links)
	}
	if !sameDir(links[0].HostPath, sibling) {
		t.Errorf("linked %s, want %s", links[0].HostPath, sibling)
	}
}

func TestIsInside(t *testing.T) {
	if !isInside("/a/b/c", "/a/b") {
		t.Error("/a/b/c should be inside /a/b")
	}
	if isInside("/a/bc", "/a/b") {
		t.Error("/a/bc is not inside /a/b")
	}
	if isInside("/a", "/a/b") {
		t.Error("/a is not inside /a/b")
	}
}
