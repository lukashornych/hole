//go:build e2e

// End-to-end tests: they build the real binary and start real sandboxes against a real
// container runtime. Run them with `make e2e`.
//
// The sandbox is driven by a *test agent* — a user agent plugin (~/.hole/agents/<name>)
// whose command is a shell one-liner — so nothing here needs a real agent CLI or API key.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lukashornych/hole/internal/hostenv"
)

var holeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hole-e2e-bin.")
	if err != nil {
		panic(err)
	}
	holeBinary = filepath.Join(dir, "hole")

	build := exec.Command("go", "build", "-o", holeBinary, "./cmd/hole")
	build.Dir = repoRoot()
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("could not build the hole binary: " + err.Error())
	}

	// A precondition, not a test: without a usable compose plugin every sandbox test fails the
	// same way, twenty minutes at a time.
	if err := checkDockerCompose(); err != nil {
		panic(err.Error())
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	// The test package lives at <root>/test/e2e.
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// environment builds an isolated HOME with a test agent, plus a project directory.
func environment(t *testing.T, agentCommand, projectSettings string) (home, projectDir string) {
	t.Helper()
	home = t.TempDir()

	// Resolved up front, because `hole start` resolves symlinks before deriving the project
	// name it labels resources with, and on macOS a temp directory sits under the
	// /var -> /private/var symlink. Asserting against the unresolved path would compute a
	// different hash and match nothing at all — a leak check that can never fail.
	projectDir, err := hostenv.ResolveProjectDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(home, ".hole", "agents", "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agentDir, "command.json"), agentCommand)
	write(t, filepath.Join(agentDir, "allow.txt"), "example.com\n")
	write(t, filepath.Join(agentDir, "install-user.sh"), "#!/bin/bash\nexit 0\n")

	// Only the test agent is installed: building the real agent CLIs would make every e2e
	// run download Node and three vendor installers.
	settings := projectSettings
	if settings == "" {
		settings = `{"container":{"enabledAgents":["test-agent"]}}`
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".hole"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(projectDir, ".hole", "settings.json"), settings)
	write(t, filepath.Join(projectDir, "README.md"), "# demo\n")
	return home, projectDir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// holeEnv is the environment for a `hole` child process: an isolated HOME so the test owns
// ~/.hole, but the real docker client configuration.
//
// HOME is also where the docker CLI looks for its cli-plugins and contexts. On macOS the compose
// plugin is installed per-user at ~/.docker/cli-plugins, so isolating HOME alone makes
// `docker compose` — and therefore every sandbox — unavailable. Linux CI does not notice: there
// the plugin sits in a system directory.
//
// An explicit DOCKER_CONFIG is honored, so a developer can point the suite at a different client
// configuration.
func holeEnv(home string) []string {
	env := append(os.Environ(), "HOME="+home)
	if os.Getenv("DOCKER_CONFIG") == "" {
		if realHome, err := os.UserHomeDir(); err == nil {
			env = append(env, "DOCKER_CONFIG="+filepath.Join(realHome, ".docker"))
		}
	}
	return env
}

// checkDockerCompose verifies the docker CLI is usable under the environment the harness hands
// each `hole` child, which is what `engine.Detect` probes first.
func checkDockerCompose() error {
	dir, err := os.MkdirTemp("", "hole-e2e-precheck.")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := exec.Command("docker", "compose", "version")
	cmd.Env = holeEnv(dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose is not usable under the test environment "+
			"(DOCKER_CONFIG=%s): %w\n%s", dockerConfigDir(), err, output)
	}
	return nil
}

// dockerConfigDir reports the client configuration directory holeEnv passes on, for diagnostics.
func dockerConfigDir() string {
	if explicit := os.Getenv("DOCKER_CONFIG"); explicit != "" {
		return explicit
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return "<unresolved>"
	}
	return filepath.Join(realHome, ".docker")
}

// TestDockerClientIsUsableUnderTheTestEnvironment is the regression test for an over-isolated
// HOME hiding the compose plugin: HOME is where the docker CLI looks for its cli-plugins and
// contexts, and on macOS the compose plugin is installed per-user under ~/.docker/cli-plugins.
// Linux CI cannot catch this — there the plugin sits in a system directory.
func TestDockerClientIsUsableUnderTheTestEnvironment(t *testing.T) {
	if err := checkDockerCompose(); err != nil {
		t.Fatal(err)
	}
}

func runHole(t *testing.T, home string, timeout time.Duration, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(holeBinary, args...)
	cmd.Env = holeEnv(home)
	output, err := runWithTimeout(cmd, timeout)
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running hole %v: %v\n%s", args, err, output)
		}
	}
	return output, code
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	var builder strings.Builder
	cmd.Stdout = &builder
	cmd.Stderr = &builder
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return builder.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return builder.String(), contextDeadlineExceeded{}
	}
}

type contextDeadlineExceeded struct{}

func (contextDeadlineExceeded) Error() string { return "timed out" }

func asExitError(err error, target **exec.ExitError) bool {
	if typed, ok := err.(*exec.ExitError); ok {
		*target = typed
		return true
	}
	return false
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// assertNoLeftovers is the plan's hard requirement: after a sandbox exits there must be no
// container, network or volume of that project left.
func assertNoLeftovers(t *testing.T, projectDir string) {
	t.Helper()
	// The full project name, hash included — not just the directory's base name. A temp
	// directory is called something like "002" in every test of every run, and Docker's name
	// filter matches substrings, so filtering on the base alone reports an earlier aborted
	// run's orphans as this run's leak. The hash is derived from the temp path, so it differs
	// per test and per run.
	// Guards the derivation above: an unresolved path would hash differently from the one
	// `hole start` used, and every check below would then look for names that never existed.
	if resolved, err := hostenv.ResolveProjectDir(projectDir); err != nil || resolved != projectDir {
		t.Fatalf("project dir %s is not the resolved path (%s, err=%v); the leftover checks would match nothing",
			projectDir, resolved, err)
	}
	project := "hole-sandbox-" + hostenv.ProjectName(projectDir)
	for _, check := range []struct {
		what string
		args []string
	}{
		// Names rather than IDs: a leftover is only actionable if you can see which instance
		// it belonged to.
		{"containers", []string{"ps", "-a", "--filter", "name=" + project, "--format", "{{.Names}}"}},
		{"networks", []string{"network", "ls", "--filter", "name=" + project, "--format", "{{.Name}}"}},
		{"volumes", []string{"volume", "ls", "-q", "--filter", "name=hole-sandbox-docker-data-" + project}},
	} {
		if out := dockerOutput(t, check.args...); out != "" {
			t.Errorf("sandbox left %s behind: %s", check.what, out)
		}
	}
}

func TestSandboxStartsAttachesAndCleansUp(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "sleep 3; echo HOLE_E2E_AGENT_RAN"]`, "")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "HOLE_E2E_AGENT_RAN") {
		t.Errorf("the agent command did not run:\n%s", output)
	}
	if !strings.Contains(output, "Sandbox destroyed") {
		t.Errorf("teardown did not complete:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestDefaultDenyBlocksUnlistedDomain(t *testing.T) {
	// With empty settings nothing but the agent's own allow list resolves, and the failure
	// must be a fast NXDOMAIN rather than a timeout.
	command := `["bash", "-c", "sleep 3; getent hosts example.org && echo RESOLVED_UNEXPECTED || echo BLOCKED_AS_EXPECTED"]`
	home, projectDir := environment(t, command, "")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "BLOCKED_AS_EXPECTED") {
		t.Errorf("an unlisted domain resolved inside the sandbox:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestAllowedDomainResolves(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["example.com"]}}`
	command := `["bash", "-c", "sleep 3; getent hosts example.com >/dev/null && echo ALLOWED_RESOLVED || echo ALLOWED_BLOCKED"]`
	home, projectDir := environment(t, command, settings)

	// `network.allow` in a project file is gated, and an e2e run has no terminal to confirm on.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "ALLOWED_RESOLVED") {
		t.Errorf("an allowed domain did not resolve:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestNetworkAccessDumpIsWritten(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["example.com"]}}`
	command := `["bash", "-c", "getent hosts example.com >/dev/null; getent hosts blocked.invalid >/dev/null; true"]`
	home, projectDir := environment(t, command, settings)

	_, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "-n", "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d", code)
	}

	entries, err := filepath.Glob(filepath.Join(projectDir, ".hole", "logs", "network-access-test-agent-*.log"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one network access log, got %v (%v)", entries, err)
	}
	content, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "ALLOWED example.com") {
		t.Errorf("dump is missing the allowed domain:\n%s", text)
	}
	if !strings.Contains(text, "DENIED blocked.invalid") {
		t.Errorf("dump is missing the denied domain:\n%s", text)
	}
}

func TestExcludedFilesAreHidden(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"files":{"exclude":[".env","secrets"]}}`
	command := `["bash", "-c", "sleep 3; [[ -s .env ]] && echo LEAKED_ENV || echo ENV_HIDDEN; [[ -n \"$(ls -A secrets)\" ]] && echo LEAKED_DIR || echo DIR_HIDDEN"]`
	home, projectDir := environment(t, command, settings)
	write(t, filepath.Join(projectDir, ".env"), "SECRET=value\n")
	if err := os.MkdirAll(filepath.Join(projectDir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(projectDir, "secrets", "key.pem"), "private\n")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "ENV_HIDDEN") || !strings.Contains(output, "DIR_HIDDEN") {
		t.Errorf("excluded paths were visible inside the sandbox:\n%s", output)
	}
	// The host files must be untouched by the over-mounts.
	if content, err := os.ReadFile(filepath.Join(projectDir, ".env")); err != nil || !strings.Contains(string(content), "SECRET") {
		t.Error("the excluded host file was modified")
	}
	assertNoLeftovers(t, projectDir)
}

func TestPrestartAndHostHooksRun(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "sleep 3; cat /tmp/prestart-marker"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "prestart": [{"script": ".hole/prestart.sh"}],
	    "setupHost": [{"script": ".hole/setup-host.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup-host.sh"}]
	  }
	}`)
	write(t, filepath.Join(projectDir, ".hole", "prestart.sh"), "#!/bin/bash\necho PRESTART_RAN > /tmp/prestart-marker\n")
	write(t, filepath.Join(projectDir, ".hole", "setup-host.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "setup-host-ran")+"\"\n")
	write(t, filepath.Join(projectDir, ".hole", "cleanup-host.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "cleanup-host-ran")+"\"\n")

	// --trust-project: these settings run scripts on the host, and a test has no terminal to
	// confirm that on.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "PRESTART_RAN") {
		t.Errorf("prestart hook did not run:\n%s", output)
	}
	for _, marker := range []string{"setup-host-ran", "cleanup-host-ran"} {
		if _, err := os.Stat(filepath.Join(projectDir, marker)); err != nil {
			t.Errorf("host hook marker %s missing", marker)
		}
	}
	assertNoLeftovers(t, projectDir)
}

func TestSetupHostFailureAbortsButStillCleansUp(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "setupHost": [{"script": ".hole/fail.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup.sh"}]
	  }
	}`)
	write(t, filepath.Join(projectDir, ".hole", "fail.sh"), "#!/bin/bash\nexit 7\n")
	write(t, filepath.Join(projectDir, ".hole", "cleanup.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "cleanup-ran")+"\"\n")

	output, code := runHole(t, home, 10*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code == 0 {
		t.Errorf("a failing setupHost hook must abort startup:\n%s", output)
	}
	if strings.Contains(output, "SHOULD_NOT_RUN") {
		t.Error("the agent started despite a failing setupHost hook")
	}
	if _, err := os.Stat(filepath.Join(projectDir, "cleanup-ran")); err != nil {
		t.Error("cleanupHost did not run after the aborted startup")
	}
	assertNoLeftovers(t, projectDir)
}

func TestInvalidSettingsFailBeforeAnyDockerWork(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "true"]`, `{"container":{"nope":true}}`)
	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("invalid settings were accepted:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

// A project's own settings file is repository content, so the host-affecting keys in it need
// the user's consent. Without a terminal to ask on, the start must fail — before the setupHost
// script it asked for has run, and before cleanupHost can be replayed from a snapshot.
func TestUntrustedProjectSettingsAbortBeforeAnyHostHook(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "setupHost": [{"script": ".hole/setup-host.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup-host.sh"}]
	  }
	}`)
	for _, hook := range []string{"setup-host", "cleanup-host"} {
		write(t, filepath.Join(projectDir, ".hole", hook+".sh"),
			"#!/bin/bash\ntouch \""+filepath.Join(projectDir, hook+"-ran")+"\"\n")
	}

	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("untrusted project settings were accepted:\n%s", output)
	}
	if !strings.Contains(output, "hooks.setupHost") || !strings.Contains(output, "--trust-project") {
		t.Errorf("the refusal does not say what was asked for or how to accept it:\n%s", output)
	}
	for _, hook := range []string{"setup-host", "cleanup-host"} {
		if _, err := os.Stat(filepath.Join(projectDir, hook+"-ran")); err == nil {
			t.Errorf("%s ran for a project that was never trusted", hook)
		}
	}
	assertNoLeftovers(t, projectDir)
}

// Egress widening leaves the sandbox, so a project file that asks for nothing but
// `network.allow` is gated like any other host-reaching request.
func TestUntrustedNetworkOnlyProjectSettingsAbort(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`,
		`{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["attacker.example.com"]}}`)

	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("untrusted egress widening was accepted:\n%s", output)
	}
	if !strings.Contains(output, "network.allow") || !strings.Contains(output, "--trust-project") {
		t.Errorf("the refusal does not say what was asked for or how to accept it:\n%s", output)
	}
	if strings.Contains(output, "SHOULD_NOT_RUN") {
		t.Errorf("the agent ran for a project that was never trusted:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestDestroyRemovesProjectResources(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "true"]`, "")
	if output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir); code != 0 {
		t.Fatalf("initial start exited with %d:\n%s", code, output)
	}
	output, code := runHole(t, home, 5*time.Minute, "destroy", projectDir)
	if code != 0 {
		t.Fatalf("hole destroy exited with %d:\n%s", code, output)
	}
	assertNoLeftovers(t, projectDir)
	// The image built for this project must be gone too. Same reason as assertNoLeftovers for
	// using the hashed project name: a bare temp-dir base name matches other runs' images.
	if images := dockerOutput(t, "images", "--filter",
		"reference=hole-sandbox/agent-"+hostenv.ProjectName(projectDir),
		"--format", "{{.Repository}}:{{.Tag}}"); images != "" {
		t.Errorf("destroy left project images behind: %s", images)
	}
}

func TestDebugModeOpensShell(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo AGENT_COMMAND_RAN"]`, "")
	// -d replaces the agent command with bash; with no TTY attached it exits immediately.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "-d")
	if code != 0 && code != 1 {
		t.Fatalf("hole start -d exited with %d:\n%s", code, output)
	}
	if strings.Contains(output, "AGENT_COMMAND_RAN") {
		t.Error("debug mode still ran the agent command")
	}
	assertNoLeftovers(t, projectDir)
}

// --- watchdog matrix -------------------------------------------------------------------
//
// The plan's hard requirement is that no container or network can survive a sandbox exit.
// The watchdog covers every single-process-death mode; GC covers the double death.

// startInBackground launches a sandbox that stays up, and returns its process plus the
// instance name once the agent container is running.
func startInBackground(t *testing.T, home, projectDir string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(holeBinary, append([]string{"start", "test-agent", projectDir}, args...)...)
	cmd.Env = holeEnv(home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hole: %v", err)
	}

	// Startup registers the instance before it creates any Docker resource, so a run that has
	// not registered by now failed a preflight check. Distinguishing that from a slow image
	// build keeps a broken startup from burning the full poll on every watchdog test.
	registrationDeadline := time.Now().Add(2 * time.Minute)
	registered := false

	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		if name := runningInstance(t, home); name != "" {
			registered = true
			if dockerOutput(t, "ps", "-q", "--filter", "name="+name+"-agent-1") != "" {
				return cmd, name
			}
		} else if !registered && time.Now().After(registrationDeadline) {
			_ = cmd.Process.Kill()
			t.Fatal("hole start never registered an instance: it failed before creating any Docker resource — see its output above")
		}
		time.Sleep(time.Second)
	}
	_ = cmd.Process.Kill()
	t.Fatal("the sandbox never reached a running agent container")
	return nil, ""
}

// runningInstance reads the single registered instance name, if there is one.
func runningInstance(t *testing.T, home string) string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(home, ".hole", "instances", "*.json"))
	if err != nil || len(entries) != 1 {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(entries[0]), ".json")
}

// waitGone waits for every trace of an instance to disappear.
func waitGone(t *testing.T, home, instanceName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		containers := dockerOutput(t, "ps", "-aq", "--filter", "name="+instanceName)
		networks := dockerOutput(t, "network", "ls", "-q", "--filter", "name="+instanceName)
		_, stateErr := os.Stat(filepath.Join(home, ".hole", "instances", instanceName+".json"))
		if containers == "" && networks == "" && os.IsNotExist(stateErr) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("instance %s still has resources after %s: containers=%q networks=%q",
		instanceName, timeout,
		dockerOutput(t, "ps", "-aq", "--filter", "name="+instanceName),
		dockerOutput(t, "network", "ls", "-q", "--filter", "name="+instanceName))
}

func watchdogPID(t *testing.T, home, instanceName string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".hole", "instances", instanceName+".json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var instance struct {
		WatchdogPID int `json:"watchdogPID"`
	}
	if err := json.Unmarshal(data, &instance); err != nil {
		t.Fatalf("parse state file: %v", err)
	}
	if instance.WatchdogPID <= 0 {
		t.Fatalf("no watchdog PID recorded: %s", data)
	}
	return instance.WatchdogPID
}

// longRunningAgent keeps the sandbox up until something stops the container.
const longRunningAgent = `["bash", "-c", "while :; do sleep 1; done"]`

func TestWatchdogCleansUpAfterSIGINT(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestWatchdogCleansUpAfterKilledCLI(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	// SIGKILL: the CLI cannot run any cleanup of its own, so only the detached watchdog can.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 3*time.Minute)
}

func TestCLICleansUpWhenTheWatchdogIsKilled(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	pid := watchdogPID(t, home, instanceName)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill watchdog: %v", err)
	}
	// With the watchdog gone the CLI must fall back to tearing down itself.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal CLI: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestGCCleansUpWhenBothProcessesAreKilled(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	pid := watchdogPID(t, home, instanceName)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill watchdog: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill CLI: %v", err)
	}
	_ = cmd.Wait()

	// Nobody is left to clean up: the sandbox survives until the next start or list runs GC.
	if dockerOutput(t, "ps", "-q", "--filter", "name="+instanceName+"-agent-1") == "" {
		t.Error("the sandbox died on its own; this test no longer covers the GC path")
	}

	if _, code := runHole(t, home, 5*time.Minute, "list"); code != 0 {
		t.Fatal("hole list failed")
	}
	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestListShowsRunningSandbox(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Wait()
		waitGone(t, home, instanceName, 2*time.Minute)
	})

	output, code := runHole(t, home, 5*time.Minute, "list")
	if code != 0 {
		t.Fatalf("hole list exited with %d:\n%s", code, output)
	}
	for _, want := range []string{"INSTANCE", "test-agent", projectDir} {
		if !strings.Contains(output, want) {
			t.Errorf("hole list output is missing %q:\n%s", want, output)
		}
	}
}
