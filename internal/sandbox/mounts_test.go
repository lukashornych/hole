package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/worktree"
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

// poolFixture builds a project with a worktree pool beside it: one checkout in the pool that
// hides its own `.env` through its own settings file, and one that has no settings file.
func poolFixture(t *testing.T) (host hostenv.Host, projectDir, poolDir, hiding, plain string) {
	t.Helper()
	host = hostenv.Host{Username: "dev", Home: t.TempDir()}
	projectDir = filepath.Join(host.Home, "myapp")
	poolDir = projectDir + "-worktrees"
	hiding = filepath.Join(poolDir, "feature")
	plain = filepath.Join(poolDir, "hotfix")

	for _, file := range []string{
		filepath.Join(projectDir, ".env"),
		filepath.Join(hiding, ".env"),
		filepath.Join(hiding, ".hole", "settings.json"),
		filepath.Join(plain, ".env"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "x"
		if filepath.Base(file) == "settings.json" {
			content = `{"files": {"exclude": [".env"]}}`
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return host, projectDir, poolDir, hiding, plain
}

// The pool is one read-write mount at its root: a checkout inside it must not get a library
// mount of its own, which would nest a read-only mount inside the read-write pool.
func TestPoolIsASingleReadWriteMount(t *testing.T) {
	host, projectDir, poolDir, _, _ := poolFixture(t)

	libraries, err := mergeLibraries(host, projectDir, nil,
		[]worktree.Link{{HostPath: poolDir, ReadWrite: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder := newMountBuilder(host, t.TempDir())
	if err := builder.addLibraries(libraries, projectDir); err != nil {
		t.Fatal(err)
	}
	if want := []string{poolDir + ":" + poolDir}; !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v", builder.mounts, want)
	}
	if !reflect.DeepEqual(builder.libraries, []string{poolDir + ":" + poolDir}) {
		t.Errorf("libraries = %v, want the pool mirrored onto the sidecar", builder.libraries)
	}
}

// Every checkout Hole exposes hides what its own settings file asks to hide. Inside the pool that
// is the only thing that can: the pool is mounted at its root, so `addLibraries` never looks at
// the children, and without this the project's secrets would be visible in every checkout below it.
func TestPoolWorktreeExclusionsComeFromEachWorktreesOwnSettings(t *testing.T) {
	host, projectDir, poolDir, hiding, plain := poolFixture(t)

	builder := newMountBuilder(host, t.TempDir())
	// The same order generateCompose uses: project exclusions, libraries (the pool), then the
	// over-mounts inside the pool.
	if err := builder.addExclusions(projectDir, projectDir, []string{".env"}); err != nil {
		t.Fatal(err)
	}
	libraries, err := mergeLibraries(host, projectDir, nil,
		[]worktree.Link{{HostPath: poolDir, ReadWrite: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.addLibraries(libraries, projectDir); err != nil {
		t.Fatal(err)
	}
	if err := builder.addPoolWorktreeExclusions([]string{hiding, plain}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"/dev/null:" + filepath.Join(projectDir, ".env") + ":ro",
		poolDir + ":" + poolDir,
		"/dev/null:" + filepath.Join(hiding, ".env") + ":ro",
	}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v (a checkout without settings inherits nothing)", builder.mounts, want)
	}
	// Over-mounts inside the pool must reach the sidecar too, or `docker build` there sees the file.
	if !reflect.DeepEqual(builder.exclusions, []string{want[0], want[2]}) {
		t.Errorf("exclusions = %v, want both over-mounts mirrored onto the sidecar", builder.exclusions)
	}
}

// The refactor that gave the pool children their exclusions must not have cost the library — and
// the derived worktrees outside the pool — theirs.
func TestLibraryExclusionsSurviveOnItsOwnMount(t *testing.T) {
	host := hostenv.Host{Username: "dev", Home: t.TempDir()}
	libraryDir := filepath.Join(host.Home, "sibling-worktree")
	for _, file := range []string{
		filepath.Join(libraryDir, ".env"),
		filepath.Join(libraryDir, ".hole", "settings.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "x"
		if filepath.Base(file) == "settings.json" {
			content = `{"files": {"exclude": [".env"]}}`
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	builder := newMountBuilder(host, t.TempDir())
	if err := builder.addLibraries(
		map[string]config.Library{libraryDir: {Path: libraryDir}}, host.Home); err != nil {
		t.Fatal(err)
	}
	want := []string{
		libraryDir + ":" + libraryDir + ":ro",
		"/dev/null:" + filepath.Join(libraryDir, ".env") + ":ro",
	}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v", builder.mounts, want)
	}
}

// The pool is a derived source like any other, so an explicit entry for the same path wins.
func TestExplicitLibraryBeatsThePool(t *testing.T) {
	poolDir := "/home/dev/myapp-worktrees"
	libraries, err := mergeLibraries(testHost(), "/home/dev/myapp",
		map[string]config.Library{poolDir: {Path: "/libs/pool", ReadWrite: false}},
		[]worktree.Link{{HostPath: poolDir, ReadWrite: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := libraries[poolDir]; got.Path != "/libs/pool" || got.ReadWrite {
		t.Errorf("pool library = %+v, want the configured entry to win", got)
	}
}
