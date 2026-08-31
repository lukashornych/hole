// Package worktree derives the libraries implied by a git worktree layout, so an agent
// working in one worktree can still read the repository's other checkouts, and names the
// pool directory the agent may create new worktrees in.
package worktree

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lukashornych/hole/v2/internal/logging"
)

// LinkMode controls what the derived libraries are mounted as.
type LinkMode string

const (
	// LinkReadOnly mounts related worktrees read-only. This is the default: an agent should
	// be able to read a sibling checkout without being able to change it.
	LinkReadOnly LinkMode = "ro"
	// LinkReadWrite mounts them read-write.
	LinkReadWrite LinkMode = "rw"
	// LinkOff derives nothing.
	LinkOff LinkMode = "off"
)

// Link is one derived library: a host path mounted at the same absolute path inside the
// sandbox, so tooling that records absolute paths keeps working.
type Link struct {
	HostPath  string
	ReadWrite bool
}

// poolSuffix names the pool directory beside the project: `~/projects/myapp` gets
// `~/projects/myapp-worktrees`.
const poolSuffix = "-worktrees"

// Derivation is everything a project's git layout implies for the sandbox.
type Derivation struct {
	// Links are the directories to mount as libraries.
	Links []Link
	// Pool is the worktree pool directory, empty unless pool mode applies. The directory may
	// not exist yet — naming it is this package's job, creating it the caller's.
	Pool string
	// PoolWorktrees are the checkouts that already exist inside the pool. The pool mount
	// covers them, so they get no mount of their own — only their own `files.exclude`, which
	// nothing else would apply for them.
	PoolWorktrees []string
}

// Derive returns what a project directory's git layout implies.
//
// If the project is a linked worktree, the main repository is added — a linked worktree's
// `.git` file only points at it, so without the main repo the agent cannot run git at all.
// If the project is the main repository, its linked worktrees outside the project directory
// are added, and with pool set the `<project>-worktrees` sibling is named as a read-write
// pool for worktrees the agent creates during the session.
//
// Pool mode deliberately never activates in a linked worktree: that keeps the pool a sibling
// of the project, so the pool mount can never nest with the project mount. It is also what
// git allows — from a linked worktree, `git worktree add` cannot write the main repository's
// admin files unless that repository is mounted read-write.
//
// git is an optional dependency: a missing binary, a non-repository, or any git failure means
// no links, never a failed start.
func Derive(projectDir string, mode LinkMode, pool bool) Derivation {
	if mode == LinkOff {
		return Derivation{}
	}
	if _, err := exec.LookPath("git"); err != nil {
		logging.Debug("git is not on PATH, skipping worktree links")
		return Derivation{}
	}

	// git reports physical paths, so every comparison here has to be against the resolved
	// project directory: a project reached through a symlink (anything under /tmp or
	// /var/folders on macOS, or a symlinked checkout anywhere) would otherwise never match
	// what git prints, and a plain repository would derive a link to itself.
	project := resolveDir(projectDir)

	commonDir, err := git(project, "rev-parse", "--git-common-dir")
	if err != nil {
		logging.Debug("%s is not a git repository, skipping worktree links", projectDir)
		return Derivation{}
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(project, commonDir)
	}
	commonDir = resolveDir(commonDir)

	readWrite := mode == LinkReadWrite
	mainRepo := filepath.Dir(commonDir)

	// In a linked worktree the common directory belongs to another checkout.
	if !sameDir(mainRepo, project) {
		if isInside(mainRepo, project) {
			return Derivation{}
		}
		logging.Debug("project is a linked worktree of %s", mainRepo)
		return Derivation{Links: []Link{{HostPath: mainRepo, ReadWrite: readWrite}}}
	}

	var derivation Derivation
	if pool {
		// Built from the already-resolved project directory, so it is resolved by
		// construction. Always read-write: a pool nobody can write to is worktreeLinks="ro"
		// with extra steps.
		derivation.Pool = project + poolSuffix
		derivation.Links = append(derivation.Links, Link{HostPath: derivation.Pool, ReadWrite: true})
	}

	raw, err := git(project, "worktree", "list", "--porcelain")
	if err != nil {
		// The pool does not depend on the listing, so it survives a failed one.
		return derivation
	}
	for _, line := range strings.Split(raw, "\n") {
		path, found := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !found {
			continue
		}
		path = resolveDir(path)
		// The project itself is already mounted, and a worktree inside it comes along with it.
		if sameDir(path, project) || isInside(path, project) {
			continue
		}
		// A checkout inside the pool is covered by the pool mount. Mounting it individually
		// too would put a read-only mount inside a read-write one, where which of the two the
		// agent sees depends on runtime mount ordering.
		if derivation.Pool != "" && isInside(path, derivation.Pool) {
			derivation.PoolWorktrees = append(derivation.PoolWorktrees, path)
			continue
		}
		derivation.Links = append(derivation.Links, Link{HostPath: path, ReadWrite: readWrite})
	}
	return derivation
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveDir returns path with symlinks resolved, falling back to a lexical clean when it
// cannot be resolved (a path that does not exist yet, or an unreadable parent).
func resolveDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func sameDir(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

// isInside reports whether path is below base.
func isInside(path, base string) bool {
	relative, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
