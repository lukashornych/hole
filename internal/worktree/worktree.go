// Package worktree derives the libraries implied by a git worktree layout, so an agent
// working in one worktree can still read the repository's other checkouts.
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

// Derive returns the worktree-implied libraries for a project directory.
//
// If the project is a linked worktree, the main repository is added — a linked worktree's
// `.git` file only points at it, so without the main repo the agent cannot run git at all.
// If the project is the main repository, its linked worktrees outside the project directory
// are added.
//
// git is an optional dependency: a missing binary, a non-repository, or any git failure means
// no links, never a failed start.
func Derive(projectDir string, mode LinkMode) []Link {
	if mode == LinkOff {
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		logging.Debug("git is not on PATH, skipping worktree links")
		return nil
	}

	// git reports physical paths, so every comparison here has to be against the resolved
	// project directory: a project reached through a symlink (anything under /tmp or
	// /var/folders on macOS, or a symlinked checkout anywhere) would otherwise never match
	// what git prints, and a plain repository would derive a link to itself.
	project := resolveDir(projectDir)

	commonDir, err := git(project, "rev-parse", "--git-common-dir")
	if err != nil {
		logging.Debug("%s is not a git repository, skipping worktree links", projectDir)
		return nil
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
			return nil
		}
		logging.Debug("project is a linked worktree of %s", mainRepo)
		return []Link{{HostPath: mainRepo, ReadWrite: readWrite}}
	}

	raw, err := git(project, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var links []Link
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
		links = append(links, Link{HostPath: path, ReadWrite: readWrite})
	}
	return links
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
