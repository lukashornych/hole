// Package hostenv resolves everything Hole needs to know about the host it runs on:
// environment-variable expansion, the shared path resolution pipeline, the host user
// identity mirrored into the sandbox, and Hole's own directories.
package hostenv

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/lukashornych/hole/v2/internal/logging"
)

const (
	// instanceIDLength matches the bash implementation's 6-character IDs.
	instanceIDLength = 6
	// instanceIDAlphabet is the character set for instance IDs; Docker resource names
	// derived from them must stay lowercase-alphanumeric.
	instanceIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	defaultUsername = "agent"
	defaultHome     = "/home/agent"
)

var envVarPattern = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}|\$([a-zA-Z_][a-zA-Z0-9_]*)`)

// ExpandEnvVars expands `$VAR` and `${VAR}` references. An undefined variable is left
// exactly as written and reported once — a broken path is the user's config problem, not a
// reason to refuse to start.
func ExpandEnvVars(input string) string {
	return expandEnvVarsWith(input, os.LookupEnv)
}

func expandEnvVarsWith(input string, lookup func(string) (string, bool)) string {
	warned := map[string]bool{}
	return envVarPattern.ReplaceAllStringFunc(input, func(match string) string {
		groups := envVarPattern.FindStringSubmatch(match)
		name := groups[1]
		if name == "" {
			name = groups[2]
		}
		if value, ok := lookup(name); ok {
			return value
		}
		if !warned[name] {
			warned[name] = true
			logging.Warn("undefined environment variable '%s', leaving unexpanded", match)
		}
		return match
	})
}

// StripTrailingSlashes removes trailing separators without turning the root into "".
func StripTrailingSlashes(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" && strings.HasPrefix(path, "/") {
		return "/"
	}
	return trimmed
}

// Host describes the host identity mirrored into the sandbox container.
type Host struct {
	Username string
	Home     string
	// UID and GID are only populated on Linux: Docker Desktop and OrbStack remap IDs
	// themselves, and forcing the host IDs there breaks the container user.
	UID string
	GID string
}

// DetectHost reads the host user identity.
func DetectHost() Host {
	host := Host{
		Username: firstNonEmpty(os.Getenv("USER"), defaultUsername),
		Home:     firstNonEmpty(os.Getenv("HOME"), defaultHome),
	}
	if runtime.GOOS == "linux" {
		host.UID = fmt.Sprint(os.Getuid())
		host.GID = fmt.Sprint(os.Getgid())
	}
	return host
}

// BuildUID returns the UID to pass as a build argument (the image default on non-Linux).
func (h Host) BuildUID() string { return firstNonEmpty(h.UID, "1000") }

// BuildGID returns the GID to pass as a build argument (the image default on non-Linux).
func (h Host) BuildGID() string { return firstNonEmpty(h.GID, "1000") }

// ResolveHostPath runs a raw settings value through the host path pipeline: trailing
// slashes stripped, environment variables expanded, `~/` against the host home, relative
// paths against baseDir.
func (h Host) ResolveHostPath(raw, baseDir string) string {
	path := ExpandEnvVars(StripTrailingSlashes(raw))
	switch {
	case path == "~":
		return h.Home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(h.Home, strings.TrimPrefix(path, "~/"))
	case path == "":
		return baseDir
	case !strings.HasPrefix(path, "/"):
		return filepath.Join(baseDir, path)
	default:
		return path
	}
}

// ResolveContainerPath is the container-side counterpart: `~/` expands against the
// sandbox home, and relative paths resolve against the project directory (which is
// mounted at its host absolute path).
func (h Host) ResolveContainerPath(raw, projectDir string) string {
	path := ExpandEnvVars(StripTrailingSlashes(raw))
	switch {
	case path == "~":
		return h.Home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(h.Home, strings.TrimPrefix(path, "~/"))
	case path == "":
		return projectDir
	case !strings.HasPrefix(path, "/"):
		return filepath.Join(projectDir, path)
	default:
		return path
	}
}

// HoleDir is Hole's per-user state directory (~/.hole).
func (h Host) HoleDir() string { return filepath.Join(h.Home, ".hole") }

// GlobalSettingsFile is the path of the global settings document.
func (h Host) GlobalSettingsFile() string { return filepath.Join(h.HoleDir(), "settings.json") }

// UserAgentsDir is where user-defined agent plugins live.
func (h Host) UserAgentsDir() string { return filepath.Join(h.HoleDir(), "agents") }

// InstancesDir holds the state file of every running sandbox.
func (h Host) InstancesDir() string { return filepath.Join(h.HoleDir(), "instances") }

// LogDir holds per-run log files.
func (h Host) LogDir() string { return filepath.Join(h.HoleDir(), "logs") }

// TmpRoot is the parent of all per-run temp directories. It lives under $HOME rather than
// $TMPDIR because Colima/Lima/Podman-Machine VMs share $HOME but not /var/folders, and
// generated files in there must be bind-mountable into containers.
func (h Host) TmpRoot() string { return filepath.Join(h.HoleDir(), "tmp") }

// CreateRunTmpDir creates a fresh per-run temp directory.
func (h Host) CreateRunTmpDir() (string, error) {
	root := h.TmpRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, "run.")
	if err != nil {
		return "", fmt.Errorf("create run temp directory: %w", err)
	}
	return dir, nil
}

// ResolveProjectDir turns a user-supplied project path into an absolute, existing directory.
func ResolveProjectDir(target string) (string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve path '%s': %w", target, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("directory '%s' does not exist", target)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("'%s' is not a directory", target)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil
	}
	return resolved, nil
}

// ProjectName derives the stable Docker-safe project name from an absolute project path:
// the sanitized basename plus the first 8 hex characters of the sha1 of the path itself.
//
// The hash is taken over the *unsanitized* path deliberately. Sanitizing first discards the
// characters that distinguish `~/my_project` from `~/myproject`, and the name is a resource
// prefix `hole destroy` force-removes by — so a collision tears down another project's running
// sandbox.
func ProjectName(absPath string) string {
	base := sanitizeName(filepath.Base(absPath))
	sum := sha1.Sum([]byte(absPath))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// sanitizeName mirrors the bash pipeline: drop leading slashes, turn separators into
// dashes, lowercase, then keep only [a-z0-9-].
func sanitizeName(input string) string {
	value := strings.TrimLeft(input, "/")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ToLower(value)
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// InstanceID returns a fresh random instance ID. crypto/rand replaces the
// `tr -dc < /dev/urandom` pipeline, which produced empty IDs on some WSL setups.
func InstanceID() (string, error) {
	buf := make([]byte, instanceIDLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	out := make([]byte, instanceIDLength)
	for i, b := range buf {
		out[i] = instanceIDAlphabet[int(b)%len(instanceIDAlphabet)]
	}
	return string(out), nil
}

// InstanceName is the compose project name and resource prefix for one sandbox run.
func InstanceName(projectName, instanceID string) string {
	return "hole-sandbox-" + projectName + "-" + instanceID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
