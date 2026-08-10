// Package version reports the running Hole build. Release discovery and self-update live in
// internal/update, which needs the release assets as well.
package version

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// DevelopmentVersion is reported by a build that carries no identity at all: a plain `go build`
// from a checkout, which skips update checks, refuses to self-update and runs no migrations.
const DevelopmentVersion = "development"

// Kind classifies how the running binary came to be. The three cases differ in what Hole may do
// on their behalf, which is why they cannot stay collapsed into "stamped or not": only a release
// may replace itself with another release, while anything with a version — a `go install` build
// included — has to run the version-change cleanup.
type Kind int

const (
	// Development is an unstamped build with no module version of its own.
	Development Kind = iota
	// Source is an unstamped build whose version comes from the module info the toolchain
	// embeds, i.e. one produced by `go install`.
	Source
	// Release is a build stamped through `-ldflags` by GoReleaser or `VERSION=… make build`.
	Release
)

// String names the kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case Source:
		return "source"
	case Release:
		return "release"
	default:
		return "development"
	}
}

// Version is stamped at build time via -ldflags. An unstamped build falls back to the module
// version recorded in the binary, and to DevelopmentVersion when there is none.
var Version = DevelopmentVersion

// current is resolved once at startup; Version mirrors its version for existing callers.
var current build

// build is the resolved identity of the running binary.
type build struct {
	version     string
	kind        Kind
	revision    string
	modified    bool
	packagePath string
}

func init() {
	info, _ := debug.ReadBuildInfo()
	current = resolve(Version, info)
	Version = current.version
}

// resolve derives the identity from the -ldflags stamp and the embedded build info. A stamp wins
// outright; without one, a module version means the binary came from `go install`, and its
// absence — reported as an empty string or `(devel)` — means a local build.
func resolve(stamped string, info *debug.BuildInfo) build {
	resolved := build{version: stamped, kind: Release}
	if info != nil {
		resolved.packagePath = info.Path
	}
	if stamped != DevelopmentVersion {
		return resolved
	}

	resolved.kind = Development
	if info == nil {
		return resolved
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			resolved.revision = setting.Value
		case "vcs.modified":
			resolved.modified = setting.Value == "true"
		}
	}
	moduleVersion := strings.TrimPrefix(info.Main.Version, "v")
	if moduleVersion == "" || moduleVersion == "(devel)" {
		return resolved
	}
	resolved.kind = Source
	resolved.version = moduleVersion
	return resolved
}

// BuildKind reports how this binary was built.
func BuildKind() Kind { return current.kind }

// CanSelfUpdate reports whether `hole update` may replace this binary. Only a release build may: a
// `go install` build is upgraded by re-running `go install`, and a checkout must never overwrite
// itself with a release.
func CanSelfUpdate() bool { return current.kind == Release }

// CanCompare reports whether Version can be compared against a release version. A pseudo-version,
// which is what a `@main` or `@<sha>` install resolves to, carries no comparable number.
func CanCompare() bool {
	if current.kind == Development {
		return false
	}
	return !strings.ContainsAny(current.version, "-+")
}

// CanMigrate reports whether the version-change cleanup may run. Anything with an identity
// qualifies, so a `go install` upgrade from 1.x still gets it; a checkout does not, because
// iterating on the code must not sweep Docker resources or rewrite the recorded version.
func CanMigrate() bool { return current.kind != Development }

// Display is the version as `hole version` prints it, annotated with how the binary was built so
// that a source build is not mistaken for a release.
func Display() string {
	switch current.kind {
	case Source:
		return current.version + " (go install)"
	case Development:
		if current.revision == "" {
			return DevelopmentVersion
		}
		revision := current.revision
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if current.modified {
			return fmt.Sprintf("%s (%s, dirty)", DevelopmentVersion, revision)
		}
		return fmt.Sprintf("%s (%s)", DevelopmentVersion, revision)
	default:
		return current.version
	}
}

// GoInstallCommand returns the `go install` line that installs the newest release of the given
// major version, derived from the package path recorded in the binary so it cannot drift from the
// module path. It returns an empty string when the path is unknown, or when a major above 1 is
// asked for and there is no `/vN` element to rewrite.
func GoInstallCommand(major int) string {
	return goInstallCommand(current.packagePath, major)
}

func goInstallCommand(packagePath string, major int) string {
	if packagePath == "" {
		return ""
	}
	elements := strings.Split(packagePath, "/")
	rewritten := false
	for index := 1; index < len(elements); index++ {
		if !isMajorElement(elements[index]) {
			continue
		}
		elements[index] = "v" + strconv.Itoa(major)
		rewritten = true
		break
	}
	if !rewritten && major > 1 {
		return ""
	}
	return "go install " + strings.Join(elements, "/") + "@latest"
}

// isMajorElement reports whether a path element is a major-version marker such as `v2`.
func isMajorElement(element string) bool {
	if len(element) < 2 || element[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(element[1:])
	return err == nil
}

// Major returns the major component of a version, or 0 when it has none.
func Major(value string) int { return component(numbers(value), 0) }

// GreaterThan compares dotted numeric versions; missing components count as zero. A leading `v`
// and any pre-release or build-metadata suffix are ignored, so `v2.0.0` and `2.0.0` compare equal
// — Hole's releases never carry a pre-release suffix, and treating one as equal to the release it
// precedes is safer than guessing which way round it sorts.
func GreaterThan(left, right string) bool {
	leftParts := numbers(left)
	rightParts := numbers(right)
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for i := 0; i < length; i++ {
		if component(leftParts, i) > component(rightParts, i) {
			return true
		}
		if component(leftParts, i) < component(rightParts, i) {
			return false
		}
	}
	return false
}

// numbers splits a version into its numeric components, dropping a `v` prefix and everything
// from the first pre-release or build-metadata separator.
func numbers(value string) []string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	return strings.Split(value, ".")
}

func component(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(parts[index]))
	if err != nil {
		return 0
	}
	return value
}
