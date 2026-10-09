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
		{"/host/lib", "/host/lib", "/host/lib", false},
		{"/host/lib/", "/host/lib/", "/host/lib", false},
		{"/host/lib:rw", "/host/lib", "/host/lib", true},
		{"/host/lib:/container/lib", "/host/lib", "/container/lib", false},
		{"/host/lib:/container/lib:rw", "/host/lib", "/container/lib", true},
		{"~/lib", "~/lib", "~/lib", false},
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

// A bare `--library PATH` used to land on /libs/<basename>, which broke every reference that
// records an absolute path (symlinks, go.mod replaces, IDE metadata) and forced users to spell
// out `--library PATH:PATH`. The two forms must be indistinguishable.
func TestParseLibraryFlagDefaultsToHostPath(t *testing.T) {
	host := testHost()
	for _, hostPath := range []string{"/host/lib", "~/lib", "../lib"} {
		bare, bareLibrary, err := ParseLibraryFlag(hostPath)
		if err != nil {
			t.Fatalf("ParseLibraryFlag(%q): %v", hostPath, err)
		}
		explicit, explicitLibrary, err := ParseLibraryFlag(hostPath + ":" + hostPath)
		if err != nil {
			t.Fatalf("ParseLibraryFlag(%q): %v", hostPath+":"+hostPath, err)
		}
		if bare != explicit || bareLibrary != explicitLibrary {
			t.Errorf("%q parsed as %s -> %+v, but %q as %s -> %+v",
				hostPath, bare, bareLibrary, hostPath+":"+hostPath, explicit, explicitLibrary)
		}
		if got := host.ResolveContainerPath(bareLibrary.Path, "/host/project"); got != host.ResolveHostPath(bare, "/host/project") {
			t.Errorf("%q mounts at %s, want its own host path", hostPath, got)
		}
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
	if got := libraries["/host/flagonly"]; got.Path != "/host/flagonly" {
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

// Every checkout Hole exposes hides what the global settings and its own settings file ask to
// hide. Inside the pool those are the only two sources: the pool is mounted at its root, so
// `addLibraries` never looks at the children, and without this the project's secrets would be
// visible in every checkout below it. Here no global set is configured, which isolates the
// checkout's own half of the rule.
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
		t.Errorf("mounts = %v, want %v (without a global set, a checkout without settings inherits nothing)",
			builder.mounts, want)
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

// libraryFixture builds a sibling checkout next to the project, with the files a global
// `files.exclude` would typically name, and optionally its own settings file.
func libraryFixture(t *testing.T, settingsContent string) (hostenv.Host, string) {
	t.Helper()
	host := hostenv.Host{Username: "dev", Home: t.TempDir()}
	libraryDir := filepath.Join(host.Home, "sibling")

	files := []string{
		filepath.Join(libraryDir, ".env"),
		filepath.Join(libraryDir, "secrets", "key.pem"),
		filepath.Join(libraryDir, "config", "app.yaml"),
	}
	if settingsContent != "" {
		files = append(files, filepath.Join(libraryDir, ".hole", "settings.json"))
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "x"
		if filepath.Base(file) == "settings.json" {
			content = settingsContent
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return host, libraryDir
}

// The README tells users to write secret patterns once, globally, so they cover every project.
// A sibling checkout without a settings file of its own used to hide nothing at all.
func TestLibraryGetsGlobalExclusions(t *testing.T) {
	host, libraryDir := libraryFixture(t, "")

	builder := newMountBuilder(host, t.TempDir())
	builder.globalExclude = []string{".env"}
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

// The checkout's own file adds to the global set instead of replacing it — and is still honored
// for `files.exclude` only, so the `libraries` it also declares mounts nothing.
func TestLibraryCombinesGlobalAndOwnExclusions(t *testing.T) {
	host, libraryDir := libraryFixture(t,
		`{"files": {"exclude": ["secrets"]}, "libraries": {"/somewhere/else": "/libs/nested"}}`)

	runTmpDir := t.TempDir()
	builder := newMountBuilder(host, runTmpDir)
	builder.globalExclude = []string{".env"}
	if err := builder.addLibraries(
		map[string]config.Library{libraryDir: {Path: libraryDir}}, host.Home); err != nil {
		t.Fatal(err)
	}

	want := []string{
		libraryDir + ":" + libraryDir + ":ro",
		"/dev/null:" + filepath.Join(libraryDir, ".env") + ":ro",
		filepath.Join(runTmpDir, "excluded-dirs", libraryDir, "secrets") + ":" +
			filepath.Join(libraryDir, "secrets"),
	}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v", builder.mounts, want)
	}
}

// An entry listed both globally and in the checkout's own file must not produce the same
// over-mount twice; the builder's one-mount-per-target rule is what guarantees it.
func TestLibraryDeduplicatesGlobalAndOwnExclusion(t *testing.T) {
	host, libraryDir := libraryFixture(t, `{"files": {"exclude": [".env"]}}`)

	builder := newMountBuilder(host, t.TempDir())
	builder.globalExclude = []string{".env"}
	if err := builder.addLibraries(
		map[string]config.Library{libraryDir: {Path: libraryDir}}, host.Home); err != nil {
		t.Fatal(err)
	}

	overMount := "/dev/null:" + filepath.Join(libraryDir, ".env") + ":ro"
	if want := []string{libraryDir + ":" + libraryDir + ":ro", overMount}; !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v", builder.mounts, want)
	}
	if want := []string{overMount}; !reflect.DeepEqual(builder.exclusions, want) {
		t.Errorf("exclusions = %v, want the over-mount mirrored exactly once", builder.exclusions)
	}
}

// The other half of the rule, unchanged: the project's settings file is repository content and
// must not govern an unrelated checkout, or a sibling and a pool child would hide different sets.
func TestProjectExclusionsStillDoNotReachLibraries(t *testing.T) {
	host, libraryDir := libraryFixture(t, "")
	projectDir := filepath.Join(host.Home, "myapp")
	if err := os.MkdirAll(filepath.Join(projectDir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config", "app.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	builder := newMountBuilder(host, t.TempDir())
	if err := builder.addExclusions(projectDir, projectDir, []string{"config/*.yaml"}); err != nil {
		t.Fatal(err)
	}
	if err := builder.addLibraries(
		map[string]config.Library{libraryDir: {Path: libraryDir}}, projectDir); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"/dev/null:" + filepath.Join(projectDir, "config", "app.yaml") + ":ro",
		libraryDir + ":" + libraryDir + ":ro",
	}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v (the project's patterns must not reach the library)", builder.mounts, want)
	}
}

// A checkout inside the pool is reached the same way a library is, so the global set covers the
// one that has no settings file of its own too.
func TestPoolWorktreesGetGlobalExclusions(t *testing.T) {
	host, projectDir, poolDir, hiding, plain := poolFixture(t)

	builder := newMountBuilder(host, t.TempDir())
	builder.globalExclude = []string{".env"}
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
		poolDir + ":" + poolDir,
		"/dev/null:" + filepath.Join(hiding, ".env") + ":ro",
		"/dev/null:" + filepath.Join(plain, ".env") + ":ro",
	}
	if !reflect.DeepEqual(builder.mounts, want) {
		t.Errorf("mounts = %v, want %v (the checkout without settings is covered globally)", builder.mounts, want)
	}
}
