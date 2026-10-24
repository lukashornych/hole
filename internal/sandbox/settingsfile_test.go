package sandbox

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hooks"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSettingsFileOverrideKeepsTildeOnTheRealHome is the regression for issue #46: replacing the
// global settings must not change what `~` means in the project settings, which is what the
// `HOME=<dir> hole …` workaround broke.
func TestSettingsFileOverrideKeepsTildeOnTheRealHome(t *testing.T) {
	realHome := t.TempDir()
	projectDir := t.TempDir()
	override := filepath.Join(t.TempDir(), "wrapper-settings.json")

	writeFile(t, filepath.Join(realHome, ".hole", "settings.json"), `{"dependencies":["from-default"]}`)
	writeFile(t, override, `{"dependencies":["from-override"]}`)
	writeFile(t, projectSettingsFile(projectDir), `{"files":{"include":{"~/.secrets/x":{"path":"/secrets/x"}}}}`)
	writeFile(t, filepath.Join(realHome, ".secrets", "x"), "secret")

	host := hostenv.Host{Home: realHome, GlobalSettingsOverride: override}
	documents, err := loadSettingsDocument(host, projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents.files) == 0 || documents.files[0] != override {
		t.Fatalf("global document should come from %s, files = %v", override, documents.files)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings.Dependencies, ",") != "from-override" {
		t.Errorf("dependencies = %v, want only the override's", settings.Dependencies)
	}

	builder := newMountBuilder(host)
	if err := builder.addIncludes(settings.Files.Include, projectDir); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realHome, ".secrets", "x") + ":/secrets/x"
	if len(builder.mounts) != 1 || builder.mounts[0] != want {
		t.Errorf("mounts = %v, want [%s]", builder.mounts, want)
	}
}

// captureLogs redirects the package logger into a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// HOLE_PROJECT_SETTINGS_FILE replaces the in-repo file, its files.exclude included, and is still
// merged with whichever global document is in effect.
func TestProjectSettingsFileOverrideReplacesProjectFile(t *testing.T) {
	tests := map[string]struct {
		globalOverride bool
	}{
		"default global file":     {globalOverride: false},
		"with HOLE_SETTINGS_FILE": {globalOverride: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			projectDir := t.TempDir()
			override := filepath.Join(t.TempDir(), "wrapper-project.json")
			globalFile := filepath.Join(home, ".hole", "settings.json")
			host := hostenv.Host{Home: home, ProjectSettingsOverride: override}
			if test.globalOverride {
				globalFile = filepath.Join(t.TempDir(), "wrapper-global.json")
				host.GlobalSettingsOverride = globalFile
			}

			writeFile(t, globalFile, `{"dependencies":["from-global"],"container":{"memoryLimit":"1g"}}`)
			writeFile(t, projectSettingsFile(projectDir),
				`{"dependencies":["from-repo"],"files":{"exclude":[".env"]},"container":{"memoryLimit":"3g"}}`)
			writeFile(t, override, `{"dependencies":["from-override"],"container":{"memoryLimit":"2g"}}`)
			logs := captureLogs(t)

			documents, err := loadSettingsDocument(host, projectDir, "")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(documents.files, ",") != globalFile+","+override {
				t.Errorf("files = %v, want [%s %s]", documents.files, globalFile, override)
			}
			project, err := config.Decode(documents.project)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(project.Dependencies, ",") != "from-override" {
				t.Errorf("project document dependencies = %v, want only the override's", project.Dependencies)
			}
			settings, err := config.Decode(documents.merged)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(settings.Dependencies, ",") != "from-global,from-override" {
				t.Errorf("dependencies = %v, want the global's then the override's", settings.Dependencies)
			}
			if settings.Container.MemoryLimit != "2g" {
				t.Errorf("memoryLimit = %q, want the override's 2g", settings.Container.MemoryLimit)
			}
			// Strict replacement: the repository's own exclusions no longer apply.
			if len(settings.Files.Exclude) != 0 {
				t.Errorf("files.exclude = %v, the ignored in-repo file must not contribute", settings.Files.Exclude)
			}
			if !strings.Contains(logs.String(), "ignoring "+projectSettingsFile(projectDir)) {
				t.Errorf("the ignored in-repo file is not reported:\n%s", logs.String())
			}
		})
	}
}

func TestProjectSettingsFileOverrideResolvesProfilesAcrossFiles(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	override := filepath.Join(t.TempDir(), "wrapper-project.json")
	writeFile(t, filepath.Join(home, ".hole", "settings.json"),
		`{"profiles":{"base":{"dependencies":["from-base"]}}}`)
	writeFile(t, override, `{"profiles":{"child":{"extends":"base","dependencies":["from-child"]}}}`)

	host := hostenv.Host{Home: home, ProjectSettingsOverride: override}
	documents, err := loadSettingsDocument(host, projectDir, "child")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings.Dependencies, ",") != "from-base,from-child" {
		t.Errorf("dependencies = %v, want the chain base -> child", settings.Dependencies)
	}
}

// Relative paths in the override resolve against the project, like in every settings file, not
// against the directory the override lives in.
func TestProjectSettingsFileOverrideResolvesRelativePathsAgainstTheProject(t *testing.T) {
	projectDir := t.TempDir()
	overrideDir := t.TempDir()
	override := filepath.Join(overrideDir, "wrapper-project.json")
	writeFile(t, override, `{"hooks":{"setup":[{"script":"setup.sh"}]}}`)
	writeFile(t, filepath.Join(projectDir, "setup.sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(overrideDir, "setup.sh"), "#!/bin/bash\n")

	host := hostenv.Host{Home: t.TempDir(), ProjectSettingsOverride: override}
	documents, err := loadSettingsDocument(host, projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}
	scripts := hooks.Resolve(settings.Hooks.Setup, host, projectDir, "setup")
	if len(scripts) != 1 || scripts[0].Path != filepath.Join(projectDir, "setup.sh") {
		t.Errorf("setup scripts = %v, want [%s]", scripts, filepath.Join(projectDir, "setup.sh"))
	}
}

// Pointing the override at the in-repo file itself is the same as not setting it.
func TestProjectSettingsFileOverrideOfTheInRepoFile(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, projectSettingsFile(projectDir), `{"files":{"exclude":[".env"]}}`)
	logs := captureLogs(t)

	host := hostenv.Host{Home: t.TempDir(), ProjectSettingsOverride: projectSettingsFile(projectDir)}
	documents, err := loadSettingsDocument(host, projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings.Files.Exclude, ",") != ".env" {
		t.Errorf("files.exclude = %v, want the in-repo file's", settings.Files.Exclude)
	}
	if strings.Contains(logs.String(), "ignoring") {
		t.Errorf("nothing was ignored, but the run says so:\n%s", logs.String())
	}
}

func TestCheckSettingsFileNotExposed(t *testing.T) {
	projectDir := t.TempDir()
	includeDir := t.TempDir()
	libraryDir := t.TempDir()
	outside := t.TempDir()

	inProject := filepath.Join(projectDir, ".wrapper", "settings.json")
	inInclude := filepath.Join(includeDir, "settings.json")
	inLibrary := filepath.Join(libraryDir, "settings.json")
	safe := filepath.Join(outside, "settings.json")
	for _, file := range []string{inProject, inInclude, inLibrary, safe} {
		writeFile(t, file, "{}")
	}
	// A path outside every mount that symlinks into the project is still exposed.
	symlinked := filepath.Join(outside, "linked.json")
	if err := os.Symlink(inProject, symlinked); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	settings.Files.Include = map[string]config.Include{includeDir: {Path: "/included"}}
	settings.Libraries = map[string]config.Library{libraryDir: {Path: "/library"}}

	tests := map[string]struct {
		settingsFile string
		wantErr      bool
	}{
		"override unset":   {settingsFile: "", wantErr: false},
		"outside":          {settingsFile: safe, wantErr: false},
		"in project":       {settingsFile: inProject, wantErr: true},
		"in include":       {settingsFile: inInclude, wantErr: true},
		"in library":       {settingsFile: inLibrary, wantErr: true},
		"symlink into dir": {settingsFile: symlinked, wantErr: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			host := hostenv.Host{Home: t.TempDir(), GlobalSettingsOverride: test.settingsFile}
			err := checkSettingsFileNotExposed(host, projectDir, settings, worktree.Derivation{}, nil)
			if (err != nil) != test.wantErr {
				t.Errorf("checkSettingsFileNotExposed() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCheckSettingsFileNotExposedCoversLibraryFlagsAndPool(t *testing.T) {
	flagLibrary := t.TempDir()
	pool := t.TempDir()
	inFlagLibrary := filepath.Join(flagLibrary, "settings.json")
	inPool := filepath.Join(pool, "settings.json")
	writeFile(t, inFlagLibrary, "{}")
	writeFile(t, inPool, "{}")

	projectDir := t.TempDir()
	host := hostenv.Host{Home: t.TempDir(), GlobalSettingsOverride: inFlagLibrary}
	if err := checkSettingsFileNotExposed(host, projectDir, &config.Settings{}, worktree.Derivation{}, []string{flagLibrary}); err == nil {
		t.Error("a settings file inside a --library mount must be refused")
	}
	host.GlobalSettingsOverride = inPool
	if err := checkSettingsFileNotExposed(host, projectDir, &config.Settings{}, worktree.Derivation{Pool: pool}, nil); err == nil {
		t.Error("a settings file inside the worktree pool must be refused")
	}
}
