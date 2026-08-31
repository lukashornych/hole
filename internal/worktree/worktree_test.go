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
	if links := Derive(t.TempDir(), LinkReadOnly, false).Links; links != nil {
		t.Errorf("a plain directory yielded %v", links)
	}
}

func TestDeriveNothingWhenOff(t *testing.T) {
	if links := Derive(repo(t), LinkOff, false).Links; links != nil {
		t.Errorf("worktreeLinks=off yielded %v", links)
	}
}

func TestDeriveNothingForAPlainRepositoryWithoutWorktrees(t *testing.T) {
	if links := Derive(repo(t), LinkReadOnly, false).Links; len(links) != 0 {
		t.Errorf("a repository with no linked worktrees yielded %v", links)
	}
}

func TestDeriveLinksSiblingWorktreeFromTheMainRepository(t *testing.T) {
	main := repo(t)
	sibling := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", sibling)

	links := Derive(main, LinkReadOnly, false).Links
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
	links := Derive(linked, LinkReadOnly, false).Links
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

	links := Derive(main, LinkReadWrite, false).Links
	if len(links) != 1 || !links[0].ReadWrite {
		t.Errorf("links = %+v, want one read-write link", links)
	}
}

func TestDeriveSkipsWorktreesInsideTheProject(t *testing.T) {
	main := repo(t)
	// A worktree below the project directory is already covered by the project mount.
	inside := filepath.Join(main, "nested", "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", inside)

	if links := Derive(main, LinkReadOnly, false).Links; len(links) != 0 {
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

	if links := Derive(link, LinkReadOnly, false).Links; len(links) != 0 {
		t.Errorf("a symlinked plain repository yielded %v", links)
	}

	sibling := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", sibling)

	links := Derive(link, LinkReadOnly, false).Links
	if len(links) != 1 {
		t.Fatalf("expected only the sibling worktree, got %v", links)
	}
	if !sameDir(links[0].HostPath, sibling) {
		t.Errorf("linked %s, want %s", links[0].HostPath, sibling)
	}
}

// linkFor returns the derived link for a host path, or fails the test.
func linkFor(t *testing.T, links []Link, hostPath string) Link {
	t.Helper()
	for _, link := range links {
		if sameDir(link.HostPath, hostPath) {
			return link
		}
	}
	t.Fatalf("no link for %s in %v", hostPath, links)
	return Link{}
}

func TestDerivePoolNamesTheProjectSiblingAndIsReadWrite(t *testing.T) {
	main := repo(t)
	derivation := Derive(main, LinkReadOnly, true)

	// The pool is named before it exists: nothing creates it here.
	want := main + "-worktrees"
	if derivation.Pool != want {
		t.Fatalf("pool = %q, want %q", derivation.Pool, want)
	}
	if _, err := os.Stat(want); err == nil {
		t.Error("Derive must not create the pool directory")
	}
	if link := linkFor(t, derivation.Links, want); !link.ReadWrite {
		t.Error("the pool must be mounted read-write")
	}
	if len(derivation.Links) != 1 {
		t.Errorf("links = %v, want the pool only", derivation.Links)
	}
}

func TestDerivePoolCoversTheWorktreesInsideIt(t *testing.T) {
	main := repo(t)
	pool := main + "-worktrees"
	inside := filepath.Join(pool, "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", inside)
	outside := filepath.Join(tempDir(t), "hotfix")
	run(t, main, "worktree", "add", "-q", "-b", "hotfix", outside)

	derivation := Derive(main, LinkReadOnly, true)

	// A checkout inside the pool must not get a second, read-only mount nested in the pool's.
	for _, link := range derivation.Links {
		if isInside(link.HostPath, pool) && !sameDir(link.HostPath, pool) {
			t.Errorf("worktree inside the pool was linked individually: %+v", link)
		}
	}
	if len(derivation.PoolWorktrees) != 1 || !sameDir(derivation.PoolWorktrees[0], inside) {
		t.Errorf("pool worktrees = %v, want [%s]", derivation.PoolWorktrees, inside)
	}
	// A checkout outside it is still linked individually, honoring the ro/rw mode.
	if link := linkFor(t, derivation.Links, outside); link.ReadWrite {
		t.Error("an outside worktree must stay read-only under worktreeLinks=ro")
	}
	if len(derivation.Links) != 2 {
		t.Errorf("links = %v, want the pool and the outside worktree", derivation.Links)
	}
}

func TestDerivePoolReadWriteModeKeepsOutsideWorktreesWritable(t *testing.T) {
	main := repo(t)
	outside := filepath.Join(tempDir(t), "hotfix")
	run(t, main, "worktree", "add", "-q", "-b", "hotfix", outside)

	derivation := Derive(main, LinkReadWrite, true)
	if link := linkFor(t, derivation.Links, outside); !link.ReadWrite {
		t.Error("worktreeLinks=rw must still apply to worktrees outside the pool")
	}
}

// The load-bearing rule: without it the pool could be a parent of the project directory, and the
// pool mount would nest with the project mount.
func TestDeriveNoPoolInALinkedWorktree(t *testing.T) {
	main := repo(t)
	linked := filepath.Join(tempDir(t), "feature")
	run(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	derivation := Derive(linked, LinkReadOnly, true)
	if derivation.Pool != "" {
		t.Errorf("pool = %q, want none in a linked worktree", derivation.Pool)
	}
	if len(derivation.Links) != 1 || !sameDir(derivation.Links[0].HostPath, main) {
		t.Errorf("links = %v, want the main repository only", derivation.Links)
	}
}

func TestDeriveNoPoolWhenLinksAreOff(t *testing.T) {
	derivation := Derive(repo(t), LinkOff, true)
	if derivation.Pool != "" || derivation.Links != nil {
		t.Errorf("worktreeLinks=off yielded %+v", derivation)
	}
}

func TestDeriveNoPoolOutsideAGitRepository(t *testing.T) {
	if derivation := Derive(t.TempDir(), LinkReadOnly, true); derivation.Pool != "" {
		t.Errorf("a plain directory yielded pool %q", derivation.Pool)
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

// bareRepo returns a bare clone at `<parent>/proj.git` together with the parent directory it
// lives in. A bare repository cannot be committed to directly, so the history comes from a
// normal repo built by repo(t); the parent also holds an unrelated checkout, which is the
// directory the old derivation used to mount.
func bareRepo(t *testing.T) (bare, parent string) {
	t.Helper()
	source := repo(t)
	parent = tempDir(t)
	bare = filepath.Join(parent, "proj.git")
	run(t, parent, "clone", "--bare", "-q", source, bare)
	run(t, parent, "clone", "-q", source, filepath.Join(parent, "seed"))
	return bare, parent
}

// notLinked fails when any derived link is at or above hostPath's directory.
func notLinked(t *testing.T, links []Link, hostPath string) {
	t.Helper()
	for _, link := range links {
		if sameDir(link.HostPath, hostPath) {
			t.Errorf("%s must not be linked, got %v", hostPath, links)
		}
	}
}

// In a linked worktree of a bare clone the common directory *is* the repository, so taking its
// parent used to mount the whole directory the bare repo happens to live in.
func TestDeriveLinksTheBareRepositoryNotItsParent(t *testing.T) {
	bare, parent := bareRepo(t)
	linked := filepath.Join(parent, "wt-feature")
	run(t, bare, "worktree", "add", "-q", "-b", "feature", linked)

	links := Derive(linked, LinkReadOnly, false).Links
	if len(links) != 1 {
		t.Fatalf("expected the bare repository, got %v", links)
	}
	if !sameDir(links[0].HostPath, bare) {
		t.Errorf("linked %s, want %s", links[0].HostPath, bare)
	}
	notLinked(t, links, parent)
}

func TestDeriveFromTheBareRepositoryLinksItsWorktrees(t *testing.T) {
	bare, parent := bareRepo(t)
	linked := filepath.Join(parent, "wt-feature")
	run(t, bare, "worktree", "add", "-q", "-b", "feature", linked)

	// The bare directory is the main repository: it is already the project mount, and its
	// parent is unrelated.
	links := Derive(bare, LinkReadOnly, false).Links
	if len(links) != 1 {
		t.Fatalf("expected the linked worktree, got %v", links)
	}
	if !sameDir(links[0].HostPath, linked) {
		t.Errorf("linked %s, want %s", links[0].HostPath, linked)
	}
	notLinked(t, links, parent)
	notLinked(t, links, bare)
}

// A bare repository has no working tree, so a pool of checkouts beside it has no convention to
// belong to — and `<project>-worktrees` would be named after the `.git` suffix.
func TestDeriveNoPoolForABareRepository(t *testing.T) {
	bare, _ := bareRepo(t)
	derivation := Derive(bare, LinkReadOnly, true)
	if derivation.Pool != "" {
		t.Errorf("pool = %q, want none for a bare repository", derivation.Pool)
	}
	if len(derivation.Links) != 0 {
		t.Errorf("links = %v, want none", derivation.Links)
	}
}

func TestDeriveNoPoolFromALinkedWorktreeOfABareRepository(t *testing.T) {
	bare, parent := bareRepo(t)
	linked := filepath.Join(parent, "wt-feature")
	run(t, bare, "worktree", "add", "-q", "-b", "feature", linked)

	derivation := Derive(linked, LinkReadOnly, true)
	if derivation.Pool != "" {
		t.Errorf("pool = %q, want none in a linked worktree", derivation.Pool)
	}
	if len(derivation.Links) != 1 || !sameDir(derivation.Links[0].HostPath, bare) {
		t.Errorf("links = %v, want the bare repository only", derivation.Links)
	}
}

func TestIsBare(t *testing.T) {
	bare, parent := bareRepo(t)
	if !isBare(bare) {
		t.Error("the bare clone was not detected as bare")
	}
	linked := filepath.Join(parent, "wt-feature")
	run(t, bare, "worktree", "add", "-q", "-b", "feature", linked)
	// rev-parse --is-bare-repository answers false here; core.bare reads the common config.
	if !isBare(linked) {
		t.Error("a linked worktree of a bare repository was not detected as bare")
	}
	if isBare(filepath.Join(parent, "seed")) {
		t.Error("a normal checkout was detected as bare")
	}
	if isBare(t.TempDir()) {
		t.Error("a plain directory was detected as bare")
	}
}
