package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

// mountBuilder accumulates bind mounts for the agent service, keeping one mount per
// container target so overlapping exclusion patterns cannot produce duplicates.
type mountBuilder struct {
	host      hostenv.Host
	runTmpDir string
	mounts    []string
	// exclusions holds the subset of mounts that hide a path rather than expose one, and
	// libraries the library mounts. Both are mirrored onto the DinD sidecar; nothing else is.
	// The daemon resolves `-v` paths in its own filesystem, so without the mirror a nested
	// container gets a silently empty directory. A mirrored `:ro` library stays read-only
	// because the daemon runs rootless: mounts it inherits into a child user namespace are
	// MNT_LOCKED, so the kernel refuses to clear MS_RDONLY. The same property is what keeps a
	// mirrored over-mount from being unmounted — being an over-mount is no boundary on its own.
	exclusions []string
	libraries  []string
	seen       map[string]bool
}

func newMountBuilder(host hostenv.Host, runTmpDir string) *mountBuilder {
	return &mountBuilder{host: host, runTmpDir: runTmpDir, seen: map[string]bool{}}
}

// add records one mount and reports whether it was kept: a target already mounted wins, so
// callers that classify their mounts must not record a rejected duplicate.
func (b *mountBuilder) add(source, target, options string) bool {
	if b.seen[target] {
		return false
	}
	b.seen[target] = true
	mount := source + ":" + target
	if options != "" {
		mount += ":" + options
	}
	b.mounts = append(b.mounts, mount)
	return true
}

// addExclusion records an over-mount, which is also mirrored onto the DinD sidecar.
func (b *mountBuilder) addExclusion(source, target, options string) {
	if !b.add(source, target, options) {
		return
	}
	b.exclusions = append(b.exclusions, b.mounts[len(b.mounts)-1])
}

// addExclusions hides paths inside sourceDir from the sandbox by over-mounting them:
// files with /dev/null, directories with an empty host directory (never an anonymous
// volume — `compose down` leaks those without -v).
func (b *mountBuilder) addExclusions(sourceDir, mountPoint string, entries []string) error {
	var resolved []string
	for _, raw := range entries {
		entry := hostenv.ExpandEnvVars(hostenv.StripTrailingSlashes(raw))
		if entry == "" {
			continue
		}
		if config.HasGlobChars(entry) {
			matches := config.ExpandGlob(sourceDir, entry)
			if len(matches) == 0 {
				logging.Warn("excluded pattern '%s' matched no paths, skipping", entry)
				continue
			}
			resolved = append(resolved, matches...)
			continue
		}
		if _, err := os.Lstat(filepath.Join(sourceDir, entry)); err != nil {
			logging.Warn("excluded path '%s' not found in %s, skipping", entry, sourceDir)
			continue
		}
		resolved = append(resolved, entry)
	}

	for _, relative := range resolved {
		relative = hostenv.StripTrailingSlashes(relative)
		if relative == "" {
			continue
		}
		fullPath := filepath.Join(sourceDir, relative)
		// Existence is checked with Lstat so a dangling symlink still warns, but the file/directory
		// decision follows the link: over-mounting /dev/null onto a symlinked directory is a mount
		// the runtime rejects, which would fail the start instead of hiding the path.
		if _, err := os.Lstat(fullPath); err != nil {
			logging.Warn("excluded path '%s' not found in %s, skipping", relative, sourceDir)
			continue
		}
		info, err := os.Stat(fullPath)
		if err != nil {
			logging.Warn("excluded path '%s' cannot be read in %s, skipping", relative, sourceDir)
			continue
		}
		target := mountPoint + "/" + relative
		if info.IsDir() {
			emptyDir := filepath.Join(b.runTmpDir, "excluded-dirs", mountPoint, relative)
			if err := os.MkdirAll(emptyDir, 0o755); err != nil {
				return fmt.Errorf("create placeholder for excluded directory '%s': %w", relative, err)
			}
			b.addExclusion(emptyDir, target, "")
			continue
		}
		b.addExclusion("/dev/null", target, "ro")
	}
	return nil
}

// addIncludes mounts extra host paths into the sandbox.
//
// Two includes resolving to the same container path are fatal rather than silently
// last-one-wins: because includes are keyed by *host* path, a base mount and a profile mount
// of different sources can collide on one target, and quietly picking one would hand the
// sandbox a different file than the settings describe.
func (b *mountBuilder) addIncludes(include map[string]string, projectDir string) error {
	targets := map[string]string{}
	for _, rawHostPath := range config.SortedKeys(include) {
		hostPath := b.host.ResolveHostPath(rawHostPath, projectDir)
		containerPath := b.host.ResolveContainerPath(include[rawHostPath], projectDir)
		if previous, clash := targets[containerPath]; clash {
			return fmt.Errorf(
				"files.include maps two host paths to the same container path '%s': '%s' and '%s'; "+
					"a mount whose source varies between profiles must live inside each profile, not in the base",
				containerPath, previous, hostPath)
		}
		targets[containerPath] = hostPath

		if _, err := os.Stat(hostPath); err != nil {
			logging.Warn("included path '%s' not found, skipping", hostPath)
			continue
		}
		b.add(hostPath, containerPath, "")
	}
	return nil
}

// ParseLibraryFlag parses one `--library PATH[:MOUNT][:rw]` value.
//
// The container path defaults to /libs/<basename> and the mount is read-only unless `:rw` is
// given, which mirrors the read-only default of configured libraries.
func ParseLibraryFlag(raw string) (hostPath string, library config.Library, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", config.Library{}, fmt.Errorf("--library needs a path")
	}

	parts := strings.Split(value, ":")
	switch len(parts) {
	case 1:
		hostPath = parts[0]
	case 2:
		hostPath = parts[0]
		if parts[1] == "rw" {
			library.ReadWrite = true
		} else {
			library.Path = parts[1]
		}
	case 3:
		hostPath = parts[0]
		library.Path = parts[1]
		if parts[2] != "rw" {
			return "", config.Library{}, fmt.Errorf(
				"invalid --library '%s': the third field must be 'rw'", raw)
		}
		library.ReadWrite = true
	default:
		return "", config.Library{}, fmt.Errorf("invalid --library '%s': expected PATH[:MOUNT][:rw]", raw)
	}

	if hostPath == "" {
		return "", config.Library{}, fmt.Errorf("invalid --library '%s': empty path", raw)
	}
	if library.Path == "" {
		library.Path = "/libs/" + filepath.Base(hostenv.StripTrailingSlashes(hostPath))
	}
	return hostPath, library, nil
}

// mergeLibraries folds the configured libraries, the git-derived ones and the --library flags
// into one map keyed by the *resolved* host path.
//
// Precedence runs derived → configured → flag, because an explicit entry for a host path must
// win over one Hole worked out on its own. Resolving before keying is what makes that hold:
// `~/other-worktree` and the derived `/home/me/other-worktree` are the same directory, and as
// two keys they would both reach `addLibraries`, where the first-wins-by-target rule silently
// picks the read-only derived one over the user's `readwrite: true`.
func mergeLibraries(host hostenv.Host, projectDir string, configured map[string]config.Library, derived []worktree.Link, flags []string) (map[string]config.Library, error) {
	libraries := map[string]config.Library{}
	for _, link := range derived {
		hostPath := host.ResolveHostPath(link.HostPath, projectDir)
		libraries[hostPath] = config.Library{Path: link.HostPath, ReadWrite: link.ReadWrite}
	}
	for rawHostPath, library := range configured {
		libraries[host.ResolveHostPath(rawHostPath, projectDir)] = library
	}
	for _, raw := range flags {
		rawHostPath, library, err := ParseLibraryFlag(raw)
		if err != nil {
			return nil, err
		}
		libraries[host.ResolveHostPath(rawHostPath, projectDir)] = library
	}
	return libraries, nil
}

// addLibraries mounts sibling projects, read-only unless the entry opts into read-write.
// A library's own settings file is honored for files.exclude only, scoped to its mount.
//
// Keys are already resolved by mergeLibraries and are not resolved again: expansion is not
// idempotent, so a directory whose name contains a literal `$` would be substituted twice.
func (b *mountBuilder) addLibraries(libraries map[string]config.Library, projectDir string) error {
	for _, hostPath := range config.SortedKeys(libraries) {
		library := libraries[hostPath]
		containerPath := b.host.ResolveContainerPath(library.Path, projectDir)

		info, err := os.Stat(hostPath)
		if err != nil || !info.IsDir() {
			logging.Warn("library '%s' not found or not a directory, skipping", hostPath)
			continue
		}
		options := "ro"
		if library.ReadWrite {
			options = ""
		}
		if b.add(hostPath, containerPath, options) {
			b.libraries = append(b.libraries, b.mounts[len(b.mounts)-1])
		}

		librarySettingsPath := filepath.Join(hostPath, ".hole", "settings.json")
		if _, err := os.Stat(librarySettingsPath); err != nil {
			continue
		}
		label := fmt.Sprintf("library settings (%s)", librarySettingsPath)
		document, err := config.LoadAndValidate(librarySettingsPath, label)
		if err != nil {
			return err
		}
		librarySettings, err := config.Decode(document)
		if err != nil {
			return err
		}
		if err := b.addExclusions(hostPath, containerPath, librarySettings.Files.Exclude); err != nil {
			return err
		}
	}
	return nil
}
