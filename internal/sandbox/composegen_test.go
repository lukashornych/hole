package sandbox

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/lukashornych/hole/internal/agents"
	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/dindregistry"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/network"
)

var updateGolden = flag.Bool("update", false, "rewrite golden compose files")

// fixture builds a project directory with files worth excluding and a library next to it.
func fixture(t *testing.T) (projectDir, libraryDir string) {
	t.Helper()
	root := t.TempDir()
	projectDir = filepath.Join(root, "demo")
	libraryDir = filepath.Join(root, "shared-lib")

	for _, file := range []string{
		filepath.Join(projectDir, ".env"),
		filepath.Join(projectDir, "secrets", "key.pem"),
		filepath.Join(projectDir, "config", "app.yaml"),
		filepath.Join(libraryDir, "lib.go"),
		filepath.Join(libraryDir, ".env"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return projectDir, libraryDir
}

func testHost() hostenv.Host {
	return hostenv.Host{Username: "dev", Home: "/home/dev", UID: "1000", GID: "1000"}
}

func testInput(t *testing.T, projectDir, runTmpDir string, settings *config.Settings, opts Options) composeInput {
	t.Helper()
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, ok := registry.Get("claude")
	if !ok {
		t.Fatal("builtin claude agent missing")
	}
	enabled, err := registry.ResolveEnabled(settings.Container.EnabledAgents)
	if err != nil {
		t.Fatal(err)
	}
	opts.ProjectDir = projectDir

	sandboxNet, err := parsePrefix("10.222.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	internetNet, err := parsePrefix("10.222.1.0/24")
	if err != nil {
		t.Fatal(err)
	}

	dindVolume := ""
	if settings.Container.Docker || opts.WithDocker {
		dindVolume = "hole-sandbox-docker-data-hole-sandbox-demo-abcd1234-xyz123"
	}

	return composeInput{
		instanceName:   "hole-sandbox-demo-abcd1234-xyz123",
		projectName:    "demo-abcd1234",
		runTmpDir:      runTmpDir,
		buildContext:   filepath.Join(runTmpDir, "agent"),
		gatewayConfDir: filepath.Join(runTmpDir, "gateway-conf"),
		prestartDir:    filepath.Join(runTmpDir, "prestart-scripts"),
		sandboxNetwork: allocatedNetwork{name: "hole-sandbox-demo-abcd1234-xyz123_sandbox", subnet: sandboxNet},
		internetNet:    allocatedNetwork{name: "hole-sandbox-demo-abcd1234-xyz123_internet", subnet: internetNet},
		gatewayIP:      "10.222.0.53",
		imageRef:       "hole-sandbox/agent-demo-abcd1234:0123456789ab",
		gatewayImage:   "hole-sandbox/gateway:0123456789ab",
		settings:       settings,
		host:           testHost(),
		startupAgent:   claude,
		enabledAgents:  enabled,
		policy:         network.BuildPolicy(nil, nil, false),
		dockerEnabled:  settings.Container.Docker || opts.WithDocker,
		dindVolume:     dindVolume,
		interactive:    true,
		opts:           opts,
	}
}

func TestGenerateComposeGolden(t *testing.T) {
	projectDir, libraryDir := fixture(t)

	tests := []struct {
		name     string
		settings *config.Settings
		opts     Options
		// nonInteractive drops the TTY, the way a piped or CI run does.
		nonInteractive bool
	}{
		{
			name:     "minimal",
			settings: &config.Settings{},
		},
		{
			name: "full",
			settings: func() *config.Settings {
				settings := &config.Settings{}
				settings.Files.Exclude = []string{".env", "secrets", "config/*.yaml"}
				settings.Files.Include = map[string]string{"~/.npmrc": "~/.npmrc"}
				settings.Libraries = map[string]config.Library{libraryDir: {Path: "/libs/shared", ReadWrite: false}}
				settings.Dependencies = []string{"make", "gcc"}
				settings.Environment = map[string]string{"MODE": "test", "ANOTHER": "value"}
				settings.Container.MemoryLimit = "8g"
				settings.Container.MemorySwapLimit = "8g"
				settings.Container.BaseImage = "ubuntu:24.04"
				settings.Container.Docker = true
				return settings
			}(),
		},
		{
			name:     "debug",
			settings: &config.Settings{},
			opts:     Options{Debug: true},
		},
		{
			name:     "agent args",
			settings: &config.Settings{},
			opts:     Options{AgentArgs: []string{"-p", "explain $HOME"}},
		},
		{
			name:           "non interactive",
			settings:       &config.Settings{},
			nonInteractive: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runTmpDir := t.TempDir()
			in := testInput(t, projectDir, runTmpDir, test.settings, test.opts)
			in.interactive = !test.nonInteractive
			path, err := generateCompose(in)
			if err != nil {
				t.Fatalf("generateCompose: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Temp paths differ per run; normalise them so the golden file stays stable.
			normalized := strings.ReplaceAll(string(data), runTmpDir, "<RUN>")
			normalized = strings.ReplaceAll(normalized, projectDir, "<PROJECT>")
			normalized = strings.ReplaceAll(normalized, libraryDir, "<LIBRARY>")

			golden := filepath.Join("testdata", strings.ReplaceAll(test.name, " ", "-")+".yml")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(normalized), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden %s: %v (run 'go test ./internal/sandbox -update')", golden, err)
			}
			if normalized != string(want) {
				t.Errorf("generated compose file differs from %s:\n--- got ---\n%s\n--- want ---\n%s", golden, normalized, want)
			}
		})
	}
}

func TestGenerateComposeMirrorsExclusionsOnDinD(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{}
	settings.Files.Exclude = []string{".env", "secrets"}
	settings.Container.Docker = true

	runTmpDir := t.TempDir()
	path, err := generateCompose(testInput(t, projectDir, runTmpDir, settings, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// `docker build` inside the sandbox must not see what the agent cannot see.
	occurrences := strings.Count(content, "/dev/null:"+projectDir+"/.env:ro")
	if occurrences != 2 {
		t.Errorf("expected the .env exclusion on both the agent and the sidecar, found %d", occurrences)
	}
}

var (
	digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	sidecarImage = regexp.MustCompile(`(?m)^\s+image: (docker[:@]\S+)$`)
)

// TestGeneratedComposePinsSidecarImageByDigest pins the generated artifact rather than the
// constant: a floating tag would let the daemon inside every sandbox change under a fixed Hole
// version, so the reference must carry a digest no matter how it is assembled.
func TestGeneratedComposePinsSidecarImageByDigest(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{}
	settings.Container.Docker = true

	runTmpDir := t.TempDir()
	path, err := generateCompose(testInput(t, projectDir, runTmpDir, settings, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := sidecarImage.FindStringSubmatch(string(data))
	if sidecar == nil {
		t.Fatalf("no Docker-in-Docker sidecar image in the compose file:\n%s", data)
	}
	if !digestPinned.MatchString(sidecar[1]) {
		t.Errorf("sidecar image %q is not pinned by digest", sidecar[1])
	}
}

// TestPinnedImagesCarryNoTag guards the other half of the pin: the digest identifies the image on
// its own, so a tag beside it is redundant information that can drift — it belongs in a comment.
func TestPinnedImagesCarryNoTag(t *testing.T) {
	for _, reference := range []string{dindImage, dindregistry.Image} {
		if !digestPinned.MatchString(reference) {
			t.Errorf("%q is not pinned by digest", reference)
			continue
		}
		if repository, _, _ := strings.Cut(reference, "@"); strings.Contains(repository, ":") {
			t.Errorf("%q carries both a tag and a digest", reference)
		}
	}
}

func TestGenerateComposeExcludesDirectoriesWithEmptyDirs(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{}
	settings.Files.Exclude = []string{"secrets"}

	runTmpDir := t.TempDir()
	path, err := generateCompose(testInput(t, projectDir, runTmpDir, settings, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Directories are hidden with an empty host directory, never an anonymous volume —
	// `compose down` does not remove those without -v.
	placeholder := filepath.Join(runTmpDir, "excluded-dirs", projectDir, "secrets")
	if !strings.Contains(string(data), placeholder+":"+projectDir+"/secrets") {
		t.Errorf("excluded directory is not backed by an empty host directory:\n%s", data)
	}
	if _, err := os.Stat(placeholder); err != nil {
		t.Errorf("placeholder directory was not created: %v", err)
	}
}

// TestGenerateComposeExcludesSymlinkedDirectoriesAsDirectories covers finding 9 of the security
// audit: classifying with Lstat put a symlinked directory on the file branch, so it got a
// /dev/null over-mount the runtime rejects — a failed start instead of a hidden path.
func TestGenerateComposeExcludesSymlinkedDirectoriesAsDirectories(t *testing.T) {
	projectDir, _ := fixture(t)
	target := filepath.Join(t.TempDir(), "real-secrets")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projectDir, "linked-secrets")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	settings := &config.Settings{}
	settings.Files.Exclude = []string{"linked-secrets"}

	runTmpDir := t.TempDir()
	path, err := generateCompose(testInput(t, projectDir, runTmpDir, settings, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := filepath.Join(runTmpDir, "excluded-dirs", projectDir, "linked-secrets")
	if !strings.Contains(string(data), placeholder+":"+projectDir+"/linked-secrets") {
		t.Errorf("a symlinked excluded directory is not backed by an empty host directory:\n%s", data)
	}
	if strings.Contains(string(data), "/dev/null:"+projectDir+"/linked-secrets") {
		t.Error("a symlinked excluded directory was over-mounted with /dev/null")
	}
}

func TestGenerateComposeSkipsMissingPaths(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{}
	settings.Files.Exclude = []string{"does-not-exist", "*.absent"}
	settings.Files.Include = map[string]string{"/absolutely/missing": "/container/missing"}
	settings.Libraries = map[string]config.Library{"/missing/library": {Path: "/libs/missing"}}

	runTmpDir := t.TempDir()
	path, err := generateCompose(testInput(t, projectDir, runTmpDir, settings, Options{}))
	if err != nil {
		t.Fatalf("missing paths must be skipped, not fatal: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"does-not-exist", "/absolutely/missing", "/missing/library"} {
		if strings.Contains(string(data), unwanted) {
			t.Errorf("missing path %q was mounted anyway", unwanted)
		}
	}
}

func TestAgentCommandExpandsSandboxHome(t *testing.T) {
	projectDir, _ := fixture(t)
	in := testInput(t, projectDir, t.TempDir(), &config.Settings{}, Options{})

	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	gemini, ok := registry.Get("gemini")
	if !ok {
		t.Fatal("builtin gemini agent missing")
	}
	in.startupAgent = gemini

	command, err := agentCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range command {
		if strings.Contains(part, "$HOME") {
			t.Errorf("command part %q still references $HOME; it must resolve to the sandbox home", part)
		}
	}
	if !strings.HasPrefix(command[0], "/home/dev/") {
		t.Errorf("command part %q was not resolved against the sandbox home", command[0])
	}
}

func TestAgentCommandDebugOverridesAgent(t *testing.T) {
	projectDir, _ := fixture(t)
	in := testInput(t, projectDir, t.TempDir(), &config.Settings{}, Options{Debug: true})
	command, err := agentCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(command) != 1 || command[0] != "bash" {
		t.Errorf("debug mode command = %v, want [bash]", command)
	}
}

func parsePrefix(value string) (netip.Prefix, error) {
	return netip.ParsePrefix(value)
}

func TestAgentCommandAppendsSettingsArgsBeforeCLIArgs(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{
		Agents: map[string]config.AgentSettings{
			"claude": {Args: []string{"--model", "opus"}},
			"gemini": {Args: []string{"--should-be-ignored"}},
		},
	}
	in := testInput(t, projectDir, t.TempDir(), settings, Options{AgentArgs: []string{"--model", "sonnet"}})

	command, err := agentCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	// Settings args come first and the CLI args last, so an ad-hoc flag wins on a repeated
	// value flag; only the started agent's args apply.
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "opus", "--model", "sonnet"}
	if !reflect.DeepEqual(command, want) {
		t.Errorf("command = %v, want %v", command, want)
	}
}

// Without a profile the merge used to run through plain config.Merge, whose array dedup drops a
// repeated flag — so the agent was launched with `--allowedTools Bash Read`, binding Read as a
// second positional instead of a second flag value.
func TestAgentCommandKeepsRepeatedFlagsWithoutAProfile(t *testing.T) {
	projectDir, _ := fixture(t)
	home := t.TempDir()
	writeSettingsFile(t, filepath.Join(home, ".hole", "settings.json"),
		`{"agents":{"claude":{"args":["--allowedTools","Bash"]}}}`)
	writeSettingsFile(t, filepath.Join(projectDir, ".hole", "settings.json"),
		`{"agents":{"claude":{"args":["--allowedTools","Read"]}}}`)

	host := hostenv.Host{Username: "dev", Home: home}
	documents, err := loadSettingsDocument(host, projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}

	in := testInput(t, projectDir, t.TempDir(), settings, Options{})
	command, err := agentCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--dangerously-skip-permissions", "--allowedTools", "Bash", "--allowedTools", "Read"}
	if !reflect.DeepEqual(command, want) {
		t.Errorf("command = %v, want %v", command, want)
	}
}

func writeSettingsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The sidecar is privileged, and a privileged process can remount a read-only bind read-write —
// so a path exposed to it is effectively writable, which would defeat the read-only default that
// makes `libraries` safe to hand to an agent. Exclusions are the opposite: mirroring an
// over-mount can only remove access. Build contexts do not need any of it, because the docker
// client streams the context to the daemon.
func TestDinDSidecarReceivesExclusionsOnly(t *testing.T) {
	projectDir, libraryDir := fixture(t)
	// A real file, so the include is actually mounted rather than warned away — otherwise the
	// assertion that it stays off the sidecar would pass for the wrong reason.
	includedFile := filepath.Join(t.TempDir(), "included-secret.txt")
	if err := os.WriteFile(includedFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := &config.Settings{}
	settings.Files.Exclude = []string{".env", "secrets"}
	settings.Files.Include = map[string]string{includedFile: "/opt/included-secret.txt"}
	settings.Libraries = map[string]config.Library{libraryDir: {Path: "/libs/shared"}}
	settings.Container.Docker = true

	in := testInput(t, projectDir, t.TempDir(), settings, Options{})
	path, err := generateCompose(in)
	if err != nil {
		t.Fatalf("generateCompose: %v", err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, sidecar, found := strings.Cut(string(generated), "\n    docker:")
	if !found {
		t.Fatal("no docker service in the generated file")
	}
	// The next service (or the top-level networks key) ends the docker block.
	if end := strings.Index(sidecar, "\n    agent:"); end != -1 {
		sidecar = sidecar[:end]
	}

	for _, exposed := range []string{libraryDir, "/libs/shared", includedFile, "/opt/included-secret.txt"} {
		if strings.Contains(sidecar, exposed) {
			t.Errorf("the privileged sidecar is given %q; only exclusion over-mounts may be mirrored", exposed)
		}
	}
	// Every exclusion must still reach it, or `docker run -v` inside the sandbox could read a
	// path the agent itself cannot see.
	for _, hidden := range []string{"/dev/null:" + projectDir + "/.env:ro", projectDir + "/secrets"} {
		if !strings.Contains(sidecar, hidden) {
			t.Errorf("exclusion %q is not mirrored onto the sidecar", hidden)
		}
	}
	if !strings.Contains(sidecar, projectDir+":"+projectDir) {
		t.Error("the project mount is missing from the sidecar; bind mounts in user compose files need it")
	}
}

// 1.x resolved these through Compose's interpolation of the generated file. Escaping `$` to stop
// Compose touching user paths removed that, so Hole expands them itself — against the host, with
// $HOME meaning the sandbox home, exactly as agent command parts already did.
func TestConfiguredValuesExpandHostVariables(t *testing.T) {
	t.Setenv("HOLE_TEST_PROJECT_PATH", "/host/projects/demo")
	projectDir, _ := fixture(t)
	settings := &config.Settings{
		Environment: map[string]string{
			"PROJECT_PATH": "$HOLE_TEST_PROJECT_PATH",
			"BRACED":       "${HOLE_TEST_PROJECT_PATH}/sub",
			"SANDBOX_HOME": "$HOME/.cache",
			"UNDEFINED":    "$HOLE_TEST_NOT_SET",
			"LITERAL":      "no-references-here",
		},
		Agents: map[string]config.AgentSettings{
			"claude": {Args: []string{"--config", "$HOLE_TEST_PROJECT_PATH/agent.json"}},
		},
	}
	in := testInput(t, projectDir, t.TempDir(), settings, Options{AgentArgs: []string{"-p", "cost of $HOLE_TEST_PROJECT_PATH"}})

	environment := userEnvironment(settings.Environment, in.host)
	want := []string{
		"BRACED=/host/projects/demo/sub",
		"LITERAL=no-references-here",
		"PROJECT_PATH=/host/projects/demo",
		"SANDBOX_HOME=" + in.host.Home + "/.cache",
		// An undefined variable stays literal and only warns, matching the path pipeline.
		"UNDEFINED=$HOLE_TEST_NOT_SET",
	}
	if !reflect.DeepEqual(environment, want) {
		t.Errorf("environment = %v, want %v", environment, want)
	}

	command, err := agentCommand(in)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{
		"claude", "--dangerously-skip-permissions",
		"--config", "/host/projects/demo/agent.json",
		// Untouched: the shell already expanded what the user left unquoted, so expanding a
		// command-line argument again would defeat their quoting.
		"-p", "cost of $HOLE_TEST_PROJECT_PATH",
	}
	if !reflect.DeepEqual(command, wantCommand) {
		t.Errorf("command = %v, want %v", command, wantCommand)
	}

	// End to end through the file, because expansion has to happen before `$` escaping: an
	// expanded value must land verbatim, and an unexpanded reference must stay escaped.
	path, err := generateCompose(in)
	if err != nil {
		t.Fatalf("generateCompose: %v", err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PROJECT_PATH=/host/projects/demo",
		"UNDEFINED=$$HOLE_TEST_NOT_SET",
		"cost of $$HOLE_TEST_PROJECT_PATH",
	} {
		if !strings.Contains(string(generated), want) {
			t.Errorf("generated compose file is missing %q", want)
		}
	}
}

func TestAgentCommandIgnoresSettingsArgsInDebugMode(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{Agents: map[string]config.AgentSettings{"claude": {Args: []string{"--model", "opus"}}}}
	in := testInput(t, projectDir, t.TempDir(), settings, Options{Debug: true})

	command, err := agentCommand(in)
	if err != nil {
		t.Fatalf("settings args with -d must be ignored, not an error: %v", err)
	}
	if !reflect.DeepEqual(command, []string{"bash"}) {
		t.Errorf("command = %v, want [bash]", command)
	}
}

func TestGenerateComposeRejectsCollidingIncludeTargets(t *testing.T) {
	projectDir, _ := fixture(t)
	settings := &config.Settings{}
	// files.include is keyed by host path, so a base mount and a profile mount of different
	// sources can target the same container path. Picking one silently would give the sandbox
	// a different file than the settings describe.
	settings.Files.Include = map[string]string{
		filepath.Join(projectDir, "config"):  "/work/config",
		filepath.Join(projectDir, "secrets"): "/work/config",
	}
	_, err := generateCompose(testInput(t, projectDir, t.TempDir(), settings, Options{}))
	if err == nil {
		t.Fatal("two includes targeting the same container path must be fatal")
	}
	if !strings.Contains(err.Error(), "/work/config") {
		t.Errorf("error should name the colliding container path: %v", err)
	}
}
