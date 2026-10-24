package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lukashornych/hole/v2/internal/compose"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

// mountBuilder accumulates bind mounts for the agent service, keeping one mount per
// container target so overlapping exclusion patterns cannot produce duplicates.
type mountBuilder struct {
	host hostenv.Host
	// mounts is the agent's complete volume list. The libraries and dockerIncludes fields below,
	// and the mount strings the masks carry, hold no mounts of their own: each records which of
	// these same strings is *also* mirrored onto the DinD sidecar, so the sidecar's set is by
	// construction a subset of the agent's — and, since `seen` already keeps one mount per
	// container target, a set with no duplicate target either.
	mounts []string
	// libraries are the library mounts and dockerIncludes only the subset of `files.include`
	// entries marked `docker` — a plain include stays off the privileged sidecar. Both are
	// mirrored onto the sidecar, as are the masks below.
	//
	// Mirroring is needed because the daemon resolves `-v` paths in its own filesystem: without it
	// a nested container gets a silently empty directory. A mirrored `:ro` library stays read-only
	// because the daemon runs rootless: mounts it inherits into a child user namespace are
	// MNT_LOCKED, so the kernel refuses to clear MS_RDONLY. The same property is what keeps a
	// mirrored over-mount from being unmounted — being an over-mount is no boundary on its own.
	libraries      []string
	dockerIncludes []string
	// masks are every path `files.exclude` hides, in the order they were recorded. It is the one
	// record of exclusions: the sidecar's mirror, the directory masks of both services, the mask
	// volume's subpaths and the targets verifyMasks checks are all derived from it, so none of
	// them can drift from the others.
	masks []mask
	// directoryMasks counts the directory masks recorded so far; it is the next one's subpath.
	directoryMasks int
	seen           map[string]bool
	// globalExclude is the global `files.exclude` (with the selected profile chain applied). It
	// is applied to every checkout that is not the project; the project mount gets its patterns
	// from the merged settings, which already contain the global ones.
	globalExclude []string
}

// mask is one path `files.exclude` hides. A file is hidden by a `/dev/null` over-mount, which is
// an ordinary entry of `mounts` recorded here by its mount string. A directory has no mount
// string: it is hidden by its own subdirectory of the sandbox's mask volume, `subpath`, or — on
// an engine that cannot mount volume subpaths — by a tmpfs.
//
// Neither directory mask has a host source. A bind whose source is a host path nested inside
// another host path is torn down about a second after the container starts by Docker Desktop's
// Docker VMM (libkrun), silently unhiding it; a volume and a tmpfs live inside the VM and
// survive. The volume is preferred because it is disk-backed: a build writing gigabytes into an
// excluded `target/` would otherwise fill RAM charged to the container's memory limit.
type mask struct {
	target  string
	mount   string
	subpath string
}

// isDir reports whether the mask hides a directory, which carries no mount string of its own.
func (m mask) isDir() bool { return m.mount == "" }

// tmpfsMaskOptions are the mount options of every tmpfs mask. The runtime's default for a tmpfs
// is `noexec,nosuid,nodev`, and an excluded directory like `node_modules` or `target` is where a
// build puts the binaries it then runs — a `noexec` mask would let the agent write them but not
// execute them, which the host placeholder the tmpfs replaced never prevented.
const tmpfsMaskOptions = "exec"

func newMountBuilder(host hostenv.Host) *mountBuilder {
	return &mountBuilder{host: host, seen: map[string]bool{}}
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

// addFileMask records a `/dev/null` over-mount of a file.
func (b *mountBuilder) addFileMask(target string) {
	if !b.add("/dev/null", target, "ro") {
		return
	}
	b.masks = append(b.masks, mask{target: target, mount: b.mounts[len(b.mounts)-1]})
}

// addDirectoryMask hides a directory behind an empty, writable directory that lives inside the
// runtime's VM. It claims the same one-per-target slot a mount does, so an exclusion and a mount
// can never both own a path. The subpath is assigned here, in recording order, so it is the same
// on every generation of the same inputs.
//
// The mask is left writable, as the empty host directory it replaced was: an exclusion like
// `node_modules` or `target` is routinely written into.
func (b *mountBuilder) addDirectoryMask(target string) {
	if b.seen[target] {
		return
	}
	b.seen[target] = true
	b.masks = append(b.masks, mask{target: target, subpath: strconv.Itoa(b.directoryMasks)})
	b.directoryMasks++
}

// exclusionMounts are the over-mounts that hide files, mirrored in full onto the DinD sidecar.
func (b *mountBuilder) exclusionMounts() []string {
	var mounts []string
	for _, mask := range b.masks {
		if !mask.isDir() {
			mounts = append(mounts, mask.mount)
		}
	}
	return mounts
}

// maskSubpaths are the subdirectories of the mask volume the directory masks mount, which have
// to exist before any container mounts them.
func (b *mountBuilder) maskSubpaths() []string {
	var subpaths []string
	for _, mask := range b.masks {
		if mask.isDir() {
			subpaths = append(subpaths, mask.subpath)
		}
	}
	return subpaths
}

// volumeMasks are the directory masks as subpath mounts of the mask volume, for the agent and the
// sidecar alike.
func (b *mountBuilder) volumeMasks(volume string) []compose.ServiceVolume {
	var entries []compose.ServiceVolume
	for _, mask := range b.masks {
		if mask.isDir() {
			entries = append(entries, compose.ServiceVolume{Source: volume, Target: mask.target, Subpath: mask.subpath})
		}
	}
	return entries
}

// tmpfsMasks are the directory masks as entries of the compose `tmpfs` key, `<path>:<options>`,
// for the agent and the sidecar alike — the fallback for an engine that cannot mount volume
// subpaths. Their pages are charged to the container's memory cgroup.
func (b *mountBuilder) tmpfsMasks() []string {
	var entries []string
	for _, mask := range b.masks {
		if mask.isDir() {
			entries = append(entries, mask.target+":"+tmpfsMaskOptions)
		}
	}
	return entries
}

// maskTargets are the container paths every exclusion hides, whichever mechanism hides them,
// for verifyMasks to assert once the container is up.
func (b *mountBuilder) maskTargets() []string {
	var targets []string
	for _, mask := range b.masks {
		targets = append(targets, mask.target)
	}
	return targets
}

// addExclusions hides paths inside sourceDir from the sandbox: files are over-mounted with
// /dev/null, directories are masked by addDirectoryMask.
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
			b.addDirectoryMask(target)
			continue
		}
		b.addFileMask(target)
	}
	return nil
}

// addIncludes mounts extra host paths into the sandbox. An entry marked `docker` is also
// recorded for the DinD sidecar, at the same container path and with the same options, so a
// container started inside the sandbox can bind-mount it directly; an unmarked entry reaches
// the agent only.
//
// Two includes resolving to the same container path are fatal rather than silently
// last-one-wins: because includes are keyed by *host* path, a base mount and a profile mount
// of different sources can collide on one target, and quietly picking one would hand the
// sandbox a different file than the settings describe.
func (b *mountBuilder) addIncludes(includes map[string]config.Include, projectDir string) error {
	targets := map[string]string{}
	for _, rawHostPath := range config.SortedKeys(includes) {
		include := includes[rawHostPath]
		hostPath := b.host.ResolveHostPath(rawHostPath, projectDir)
		containerPath := b.host.ResolveContainerPath(include.Path, projectDir)
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
		if b.add(hostPath, containerPath, "") && include.Docker {
			b.dockerIncludes = append(b.dockerIncludes, b.mounts[len(b.mounts)-1])
		}
	}
	return nil
}

// ParseLibraryFlag parses one `--library PATH[:MOUNT][:rw]` value.
//
// The container path defaults to the host path — the same place the project and the git-derived
// checkouts are mounted, so absolute paths, symlinks and inter-checkout relative references keep
// resolving inside the sandbox. The mount is read-only unless `:rw` is given, which mirrors the
// read-only default of configured libraries.
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
		library.Path = hostenv.StripTrailingSlashes(hostPath)
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
// Each library hides the global `files.exclude` plus its own settings file's, scoped to its mount.
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

		if err := b.addCheckoutExclusions(hostPath, containerPath); err != nil {
			return err
		}
	}
	return nil
}

// addCheckoutExclusions hides, inside one checkout's mount, what the global `files.exclude` and
// that checkout's own `.hole/settings.json` ask to hide. The project's `files.exclude` deliberately
// does not reach it: it is repository content, and a sibling checkout and a pool child must hide
// the same set regardless of which project opened the sandbox.
//
// Every checkout Hole exposes that is not the project goes through here — a library, a derived
// worktree, or a worktree inside the pool — so "the same way" is one implementation. The checkout's
// own file is honored for `files.exclude` only.
func (b *mountBuilder) addCheckoutExclusions(hostPath, containerPath string) error {
	entries := append([]string(nil), b.globalExclude...)

	settingsPath := filepath.Join(hostPath, ".hole", "settings.json")
	if _, err := os.Stat(settingsPath); err == nil {
		label := fmt.Sprintf("library settings (%s)", settingsPath)
		document, err := config.LoadAndValidate(settingsPath, label)
		if err != nil {
			return err
		}
		settings, err := config.Decode(document)
		if err != nil {
			return err
		}
		entries = append(entries, settings.Files.Exclude...)
	}

	if len(entries) == 0 {
		return nil
	}
	// An entry named both globally and by the checkout needs no string-level dedupe: two identical
	// patterns resolve to the same container target, which `add` keeps exactly once.
	return b.addExclusions(hostPath, containerPath, entries)
}

// addPoolWorktreeExclusions hides, in every checkout inside the worktree pool, what the global
// settings and that checkout's own settings file ask to hide.
//
// The pool is a single mount at its root, so `addLibraries` only ever looks for
// `<pool>/.hole/settings.json` — every secret in every checkout below it would otherwise be
// visible, where an equivalent checkout mounted as its own library is protected. Because the
// pool is mounted at its host path, source and mount point are the same absolute path, so
// these land as over-mounts *inside* the pool mount, exactly like the project's own exclusions.
func (b *mountBuilder) addPoolWorktreeExclusions(worktrees []string) error {
	for _, checkout := range worktrees {
		if err := b.addCheckoutExclusions(checkout, checkout); err != nil {
			return err
		}
	}
	return nil
}

// checkSettingsFileNotExposed refuses a HOLE_SETTINGS_FILE that lies inside the project or any
// path the settings would mount: includes, libraries, git-derived worktrees and the pool. The
// global document is trusted without a prompt, so an agent that can write it could add a
// `hooks.setupHost` the next start runs on the host. It runs before the trust gate and every host
// hook, so a tampered file is refused before anything in it executes.
//
// The default ~/.hole/settings.json is not checked: mounting ~/.hole is already a choice the trust
// gate sees.
func checkSettingsFileNotExposed(host hostenv.Host, projectDir string, settings *config.Settings, gitLayout worktree.Derivation, libraryFlags []string) error {
	if host.GlobalSettingsOverride == "" {
		return nil
	}
	libraries, err := mergeLibraries(host, projectDir, settings.Libraries, gitLayout.Links, libraryFlags)
	if err != nil {
		return err
	}
	sources := []string{projectDir}
	for _, rawHostPath := range config.SortedKeys(settings.Files.Include) {
		sources = append(sources, host.ResolveHostPath(rawHostPath, projectDir))
	}
	sources = append(sources, config.SortedKeys(libraries)...)
	if gitLayout.Pool != "" {
		sources = append(sources, gitLayout.Pool)
	}

	settingsFile := resolveSymlinks(host.GlobalSettingsOverride)
	for _, source := range sources {
		resolvedSource := resolveSymlinks(source)
		if settingsFile == resolvedSource || strings.HasPrefix(settingsFile, resolvedSource+string(filepath.Separator)) {
			return fmt.Errorf(
				"HOLE_SETTINGS_FILE '%s' lies inside '%s', which is mounted into the sandbox; "+
					"the agent could rewrite the global settings, so move the file outside every mounted path",
				host.GlobalSettingsOverride, source)
		}
	}
	return nil
}

// resolveSymlinks returns the symlink-free form of path, or path itself when it cannot be resolved.
func resolveSymlinks(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}
