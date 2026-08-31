package version

import (
	"runtime/debug"
	"strconv"
	"testing"
)

func TestGreaterThan(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"1.2.3", "1.2.2", true},
		{"1.3.0", "1.2.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.2", "1.2.3", false},
		{"1.2", "1.2.0", false},
		{"1.2.1", "1.2", true},
		{"1.10.0", "1.9.0", true},
		// A module version keeps its `v`, and dropping it is what stops a release from looking
		// newer than the identical version installed with `go install`.
		{"2.0.0", "v2.0.0", false},
		{"v2.1.0", "2.0.9", true},
		{"2.0.0", "v2.1.0", false},
		// Pseudo-versions and pre-releases compare on their numeric prefix alone.
		{"2.0.0", "0.0.0-20260810101741-bc2a1815dfdf", true},
		{"2.0.0", "2.0.0-rc1", false},
	}
	for _, test := range tests {
		if got := GreaterThan(test.left, test.right); got != test.want {
			t.Errorf("GreaterThan(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}

func TestMajor(t *testing.T) {
	tests := map[string]int{
		"2.1.4":                             2,
		"v3.0.0":                            3,
		"0.0.0-20260810101741-bc2a1815dfdf": 0,
		DevelopmentVersion:                  0,
	}
	for value, want := range tests {
		if got := Major(value); got != want {
			t.Errorf("Major(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestResolveClassifiesEveryBuild(t *testing.T) {
	tests := []struct {
		name        string
		stamped     string
		info        *debug.BuildInfo
		wantKind    Kind
		wantVersion string
	}{
		{
			name:        "a stamped build is a release, whatever the build info says",
			stamped:     "2.0.0",
			info:        buildInfo("(devel)"),
			wantKind:    Release,
			wantVersion: "2.0.0",
		},
		{
			name:        "an unstamped build with no build info is a development build",
			stamped:     DevelopmentVersion,
			info:        nil,
			wantKind:    Development,
			wantVersion: DevelopmentVersion,
		},
		{
			name:        "a dirty checkout reports (devel) and stays a development build",
			stamped:     DevelopmentVersion,
			info:        checkoutBuildInfo("(devel)", true),
			wantKind:    Development,
			wantVersion: DevelopmentVersion,
		},
		{
			// Go 1.24 and later derive a module version for a clean checkout too, so a
			// `make build` binary looks exactly like a `go install` one except for its VCS
			// settings. Classifying it as a source install would let a working tree run the
			// version-change migrations.
			name:        "a clean checkout keeps its VCS identity despite a module version",
			stamped:     DevelopmentVersion,
			info:        checkoutBuildInfo("v2.0.0-20260810132358-8014588bf523", false),
			wantKind:    Development,
			wantVersion: DevelopmentVersion,
		},
		{
			name:        "a tagged go install carries the release number",
			stamped:     DevelopmentVersion,
			info:        buildInfo("v2.1.4"),
			wantKind:    Source,
			wantVersion: "2.1.4",
		},
		{
			name:        "a branch go install carries a pseudo-version",
			stamped:     DevelopmentVersion,
			info:        buildInfo("v0.0.0-20260810101741-bc2a1815dfdf"),
			wantKind:    Source,
			wantVersion: "0.0.0-20260810101741-bc2a1815dfdf",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved := resolve(test.stamped, test.info)
			if resolved.kind != test.wantKind {
				t.Errorf("kind = %v, want %v", resolved.kind, test.wantKind)
			}
			if resolved.version != test.wantVersion {
				t.Errorf("version = %q, want %q", resolved.version, test.wantVersion)
			}
		})
	}
}

func TestPredicatesPerBuildKind(t *testing.T) {
	tests := []struct {
		name           string
		build          build
		wantSelfUpdate bool
		wantCompare    bool
		wantMigrate    bool
		wantDisplay    string
	}{
		{
			name:           "release",
			build:          build{version: "2.1.4", kind: Release},
			wantSelfUpdate: true,
			wantCompare:    true,
			wantMigrate:    true,
			wantDisplay:    "2.1.4",
		},
		{
			name:        "tagged go install",
			build:       build{version: "2.1.4", kind: Source},
			wantCompare: true,
			wantMigrate: true,
			wantDisplay: "2.1.4 (go install)",
		},
		{
			// Nothing to compare a pseudo-version against, but it is still an identity, so the
			// version-change cleanup a 1.x user needs still runs.
			name:        "branch go install",
			build:       build{version: "0.0.0-20260810101741-bc2a1815dfdf", kind: Source},
			wantMigrate: true,
			wantDisplay: "0.0.0-20260810101741-bc2a1815dfdf (go install)",
		},
		{
			name:        "dirty checkout",
			build:       build{version: DevelopmentVersion, kind: Development, revision: "bc2a1815dfdf1234", modified: true},
			wantDisplay: "development (bc2a181, dirty)",
		},
		{
			name:        "clean checkout",
			build:       build{version: DevelopmentVersion, kind: Development, revision: "bc2a1815dfdf1234"},
			wantDisplay: "development (bc2a181)",
		},
		{
			name:        "build without version control",
			build:       build{version: DevelopmentVersion, kind: Development},
			wantDisplay: DevelopmentVersion,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := current
			current = test.build
			t.Cleanup(func() { current = restore })

			if got := CanSelfUpdate(); got != test.wantSelfUpdate {
				t.Errorf("CanSelfUpdate = %v, want %v", got, test.wantSelfUpdate)
			}
			if got := CanCompare(); got != test.wantCompare {
				t.Errorf("CanCompare = %v, want %v", got, test.wantCompare)
			}
			if got := CanMigrate(); got != test.wantMigrate {
				t.Errorf("CanMigrate = %v, want %v", got, test.wantMigrate)
			}
			if got := Display(); got != test.wantDisplay {
				t.Errorf("Display = %q, want %q", got, test.wantDisplay)
			}
		})
	}
}

func TestGoInstallCommandRewritesTheMajorElement(t *testing.T) {
	const packagePath = "github.com/lukashornych/hole/v2/cmd/hole"
	tests := []struct {
		name  string
		path  string
		major int
		want  string
	}{
		{
			name:  "the installed major keeps the path",
			path:  packagePath,
			major: 2,
			want:  "go install github.com/lukashornych/hole/v2/cmd/hole@latest",
		},
		{
			// `@latest` never crosses a major boundary, so the command has to name the new path.
			name:  "a newer major moves the path",
			path:  packagePath,
			major: 3,
			want:  "go install github.com/lukashornych/hole/v3/cmd/hole@latest",
		},
		{
			name:  "a v1 path has nothing to rewrite",
			path:  "github.com/lukashornych/hole/cmd/hole",
			major: 1,
			want:  "go install github.com/lukashornych/hole/cmd/hole@latest",
		},
		{
			name:  "a v1 path cannot name a v2 command",
			path:  "github.com/lukashornych/hole/cmd/hole",
			major: 2,
			want:  "",
		},
		{
			name:  "no path, no command",
			path:  "",
			major: 2,
			want:  "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := goInstallCommand(test.path, test.major); got != test.want {
				t.Errorf("goInstallCommand(%q, %d) = %q, want %q", test.path, test.major, got, test.want)
			}
		})
	}
}

// buildInfo is what a module install records: a version, and no version-control settings.
func buildInfo(mainVersion string) *debug.BuildInfo {
	info := &debug.BuildInfo{Path: "github.com/lukashornych/hole/v2/cmd/hole"}
	info.Main.Version = mainVersion
	return info
}

// checkoutBuildInfo is what a build from a working tree records: the same version fields plus the
// version-control settings that identify it as local.
func checkoutBuildInfo(mainVersion string, modified bool) *debug.BuildInfo {
	info := buildInfo(mainVersion)
	info.Settings = []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "8014588bf523fbe3afe158ab38f4139721fa7ea7"},
		{Key: "vcs.modified", Value: strconv.FormatBool(modified)},
	}
	return info
}
