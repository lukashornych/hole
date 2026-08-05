package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/worktree"
)

func TestParseLibraryFlag(t *testing.T) {
	tests := []struct {
		raw           string
		wantHost      string
		wantContainer string
		wantReadWrite bool
	}{
		{"/host/lib", "/host/lib", "/libs/lib", false},
		{"/host/lib/", "/host/lib/", "/libs/lib", false},
		{"/host/lib:rw", "/host/lib", "/libs/lib", true},
		{"/host/lib:/container/lib", "/host/lib", "/container/lib", false},
		{"/host/lib:/container/lib:rw", "/host/lib", "/container/lib", true},
		{"~/lib", "~/lib", "/libs/lib", false},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			hostPath, library, err := ParseLibraryFlag(test.raw)
			if err != nil {
				t.Fatalf("ParseLibraryFlag(%q): %v", test.raw, err)
			}
			if hostPath != test.wantHost || library.Path != test.wantContainer || library.ReadWrite != test.wantReadWrite {
				t.Errorf("= %s -> %+v, want %s -> %s (rw=%v)",
					hostPath, library, test.wantHost, test.wantContainer, test.wantReadWrite)
			}
		})
	}
}

func TestParseLibraryFlagRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"", "   ", ":/container", "/host:/container:ro", "/host:/c:rw:extra"} {
		if _, _, err := ParseLibraryFlag(raw); err == nil {
			t.Errorf("ParseLibraryFlag(%q) was accepted", raw)
		}
	}
}

func TestMergeLibrariesPrecedence(t *testing.T) {
	configured := map[string]config.Library{
		"/host/shared": {Path: "/libs/configured", ReadWrite: false},
	}
	derived := []worktree.Link{
		{HostPath: "/host/shared", ReadWrite: true},
		{HostPath: "/host/worktree", ReadWrite: false},
	}
	flags := []string{"/host/shared:/libs/flag:rw", "/host/flagonly"}

	libraries, err := mergeLibraries(testHost(), "/host/project", configured, derived, flags)
	if err != nil {
		t.Fatal(err)
	}

	// A flag beats configured settings, which beat what Hole derived on its own.
	if got := libraries["/host/shared"]; got.Path != "/libs/flag" || !got.ReadWrite {
		t.Errorf("/host/shared = %+v, want the --library value to win", got)
	}
	if got := libraries["/host/worktree"]; got.Path != "/host/worktree" {
		t.Errorf("derived worktree library = %+v, want it mounted at its own path", got)
	}
	if got := libraries["/host/flagonly"]; got.Path != "/libs/flagonly" {
		t.Errorf("flag-only library = %+v", got)
	}
	if len(libraries) != 3 {
		t.Errorf("expected 3 libraries, got %d: %v", len(libraries), libraries)
	}
}

// The same directory spelled two ways used to survive as two keys, and `addLibraries` resolves
// both to one container target — where first-wins-by-target handed the mount to the read-only
// derived entry and silently dropped the user's `readwrite: true`.
func TestMergeLibrariesResolvesSpellingsOfTheSameDirectory(t *testing.T) {
	host := hostenv.Host{Username: "dev", Home: t.TempDir()}
	libraryDir := filepath.Join(host.Home, "other-worktree")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatal(err)
	}

	libraries, err := mergeLibraries(host, host.Home,
		map[string]config.Library{"~/other-worktree": {Path: libraryDir, ReadWrite: true}},
		[]worktree.Link{{HostPath: libraryDir, ReadWrite: false}},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(libraries) != 1 {
		t.Fatalf("two spellings of %s produced %d entries: %v", libraryDir, len(libraries), libraries)
	}

	builder := newMountBuilder(host, t.TempDir())
	if err := builder.addLibraries(libraries, host.Home); err != nil {
		t.Fatal(err)
	}
	want := []string{libraryDir + ":" + libraryDir}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v (the configured read-write entry must win)", builder.mounts, want)
	}
}

func TestMergeLibrariesReportsABadFlag(t *testing.T) {
	if _, err := mergeLibraries(testHost(), "/host/project", nil, nil, []string{"/host:/c:ro"}); err == nil {
		t.Error("a malformed --library value must be fatal")
	}
}

func TestMergeLibrariesConfiguredOnly(t *testing.T) {
	configured := map[string]config.Library{"/a": {Path: "/libs/a"}}
	libraries, err := mergeLibraries(testHost(), "/host/project", configured, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(libraries, configured) {
		t.Errorf("libraries = %v", libraries)
	}
}
