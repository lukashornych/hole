package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/assets"
	"github.com/lukashornych/hole/v2/internal/hooks"
)

// dockerfileRun returns the shell command of the agent Dockerfile's RUN instruction that starts
// with prefix, its continuation lines joined the way the builder joins them.
func dockerfileRun(t *testing.T, prefix string, occurrence int) string {
	t.Helper()
	data, err := fs.ReadFile(assets.Agents(), "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	seen := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "RUN "+prefix) {
			continue
		}
		if seen++; seen < occurrence {
			continue
		}
		var command []string
		for _, continued := range lines[i:] {
			trimmed, more := strings.CutSuffix(continued, "\\")
			command = append(command, trimmed)
			if !more {
				break
			}
		}
		return strings.TrimPrefix(strings.Join(command, " "), "RUN ")
	}
	t.Fatalf("no RUN %s... #%d in the agent Dockerfile", prefix, occurrence)
	return ""
}

// runBuildShell runs a command the way the Dockerfile's SHELL does.
func runBuildShell(t *testing.T, command string) (string, int) {
	t.Helper()
	output, err := exec.Command("bash", "-o", "pipefail", "-c", command).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(output), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(output), 0
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Regression: the build shell has no -e, so a loop's status was only its last script's — a
// failing setup hook followed by a succeeding one produced a green build with a broken image.
func TestSetupHookLoopFailsOnTheFirstFailingScript(t *testing.T) {
	dir := t.TempDir()
	ranSecond := filepath.Join(dir, "second-ran")
	writeScript(t, filepath.Join(dir, "001-fail.sh"), "#!/bin/bash\necho failing\nexit 1\n")
	writeScript(t, filepath.Join(dir, "002-ok.sh"), "#!/bin/bash\ntouch '"+ranSecond+"'\n")

	command := strings.ReplaceAll(dockerfileRun(t, "for script in /tmp/setup-scripts/", 1), "/tmp/setup-scripts", dir)
	output, code := runBuildShell(t, command)
	if code != 1 {
		t.Errorf("loop exited %d, want the failing script's 1:\n%s", code, output)
	}
	if _, err := os.Stat(ranSecond); err == nil {
		t.Error("the loop went on after a failing setup hook")
	}
	want := "::hole-hook-begin setup 001-fail.sh::\nfailing\n::hole-hook-end setup 001-fail.sh exit=1::\n"
	if output != want {
		t.Errorf("output %q, want %q", output, want)
	}
}

func TestSetupHookLoopSucceeds(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "001-a.sh"), "#!/bin/bash\necho a\n")
	writeScript(t, filepath.Join(dir, "002-b.sh"), "#!/bin/bash\necho b\n")
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	command := strings.ReplaceAll(dockerfileRun(t, "for script in /tmp/setup-scripts/", 1), "/tmp/setup-scripts", dir)
	output, code := runBuildShell(t, command)
	if code != 0 || !strings.Contains(output, "::hole-hook-end setup 002-b.sh exit=0::") {
		t.Errorf("exit %d, output:\n%s", code, output)
	}

	// No setup hooks at all leaves the glob unexpanded, which must not fail the build.
	empty := t.TempDir()
	if _, code := runBuildShell(t, strings.ReplaceAll(dockerfileRun(t, "for script in /tmp/setup-scripts/", 1), "/tmp/setup-scripts", empty)); code != 0 {
		t.Errorf("an empty setup hook directory failed the build with %d", code)
	}
}

// The agent install loops had the same shape as the setup loop, and the same bug.
func TestAgentInstallLoopsFailOnAnyAgent(t *testing.T) {
	for _, test := range []struct {
		name       string
		occurrence int
		script     string
	}{
		{"root phase", 1, "install-root.sh"},
		{"user phase", 3, "install-user.sh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeScript(t, filepath.Join(dir, "a-agent", test.script), "#!/bin/bash\nexit 7\n")
			writeScript(t, filepath.Join(dir, "b-agent", test.script), "#!/bin/bash\nexit 0\n")
			command := strings.ReplaceAll(dockerfileRun(t, "for agent_dir in /tmp/agent-installs/", test.occurrence), "/tmp/agent-installs", dir)
			if output, code := runBuildShell(t, command); code != 7 {
				t.Errorf("loop exited %d, want 7:\n%s", code, output)
			}
		})
	}
}

// bareWrapperLoop is the Dockerfile's wrapper install loop over agentInstalls, installing into home.
func bareWrapperLoop(t *testing.T, agentInstalls, home string) string {
	t.Helper()
	command := dockerfileRun(t, "for wrapper in /tmp/agent-installs/", 1)
	command = strings.ReplaceAll(command, "/tmp/agent-installs", agentInstalls)
	return strings.ReplaceAll(command, "${AGENT_HOME}", home)
}

func TestBareWrapperLoopInstallsWrappers(t *testing.T) {
	agentInstalls, home := t.TempDir(), t.TempDir()
	writeScript(t, filepath.Join(agentInstalls, "with-bare", "bare-command"), "#!/bin/sh\necho wrapped\n")
	writeScript(t, filepath.Join(agentInstalls, "without-bare", "install-user.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(agentInstalls, "with-bare", "bare-command"), 0o644); err != nil {
		t.Fatal(err)
	}

	if output, code := runBuildShell(t, bareWrapperLoop(t, agentInstalls, home)); code != 0 {
		t.Fatalf("loop exited %d:\n%s", code, output)
	}
	info, err := os.Stat(filepath.Join(home, ".local", "bin", "with-bare"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("wrapper installed with mode %v, want 0755", info.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "without-bare")); !os.IsNotExist(err) {
		t.Errorf("an agent without a wrapper got one installed: %v", err)
	}

	// No agent has a wrapper: the glob stays unexpanded, which must not fail the build.
	if output, code := runBuildShell(t, bareWrapperLoop(t, t.TempDir(), t.TempDir())); code != 0 {
		t.Errorf("an empty agent-installs directory failed the build with %d:\n%s", code, output)
	}
}

// A silent skip would leave a sandbox where the bare name runs something other than the file
// declares, so an existing target fails the build — a dangling symlink included.
func TestBareWrapperLoopFailsOnAnExistingTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(t *testing.T, target string)
	}{
		{"file", func(t *testing.T, target string) { writeScript(t, target, "#!/bin/sh\n") }},
		{"dangling symlink", func(t *testing.T, target string) {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/nonexistent", target); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			agentInstalls, home := t.TempDir(), t.TempDir()
			writeScript(t, filepath.Join(agentInstalls, "demo", "bare-command"), "#!/bin/sh\necho wrapped\n")
			target := filepath.Join(home, ".local", "bin", "demo")
			test.create(t, target)

			output, code := runBuildShell(t, bareWrapperLoop(t, agentInstalls, home))
			if code != 1 {
				t.Errorf("loop exited %d, want 1:\n%s", code, output)
			}
			if !strings.Contains(output, "agent 'demo'") || !strings.Contains(output, target) {
				t.Errorf("message does not name the agent and the path:\n%s", output)
			}
			if content, err := os.ReadFile(target); err == nil && strings.Contains(string(content), "wrapped") {
				t.Error("the existing target was overwritten")
			}
		})
	}
}

// entrypointWithScriptsIn is the container entrypoint reading its prestart scripts from dir.
func entrypointWithScriptsIn(t *testing.T, dir string) string {
	t.Helper()
	data, err := fs.ReadFile(assets.Agents(), "entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "entrypoint.sh")
	writeScript(t, path, strings.ReplaceAll(string(data), "/tmp/prestart-scripts", dir))
	return path
}

func TestEntrypointStopsAtTheFirstFailingPrestartScript(t *testing.T) {
	dir := t.TempDir()
	ranSecond := filepath.Join(dir, "second-ran")
	writeScript(t, filepath.Join(dir, "001-fail"), "#!/bin/bash\necho failing\nexit 2\n")
	writeScript(t, filepath.Join(dir, "002-ok"), "#!/bin/bash\ntouch '"+ranSecond+"'\n")

	cmd := exec.Command("bash", entrypointWithScriptsIn(t, dir), "echo", "agent-ran")
	cmd.Env = append(os.Environ(), "HOLE_GATEWAY_IP=")
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("entrypoint ended with %v, want exit 2:\n%s", err, output)
	}
	want := "::hole-hook-begin prestart 001-fail::\nfailing\n::hole-hook-end prestart 001-fail exit=2::\n"
	if string(output) != want {
		t.Errorf("output %q, want %q", output, want)
	}
	if _, err := os.Stat(ranSecond); err == nil {
		t.Error("a prestart script ran after a failing one")
	}
}

func TestEntrypointRunsPrestartScriptsThenTheAgent(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "001-a"), "#!/bin/bash\necho a\n")
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", entrypointWithScriptsIn(t, dir), "echo", "agent-ran")
	cmd.Env = append(os.Environ(), "HOLE_GATEWAY_IP=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v:\n%s", err, output)
	}
	want := "::hole-hook-begin prestart 001-a::\na\n::hole-hook-end prestart 001-a exit=0::\n" + agentStartLine + "\nagent-ran\n"
	if string(output) != want {
		t.Errorf("output %q, want %q", output, want)
	}
}

func TestNormalizeTerminalLine(t *testing.T) {
	tests := map[string]string{
		"plain":                                     "plain",
		"\x1b[32mgreen\x1b[0m text":                 "green text",
		"progress 10%\rprogress 50%\rdone":          "done",
		"\x1b]0;window title\x07after title":        "after title",
		"line with tty ending\r":                    "line with tty ending",
		"\r\x1b[Kredrawn":                           "redrawn",
		"::hole-hook-end prestart 001-a exit=0::\r": "::hole-hook-end prestart 001-a exit=0::",
	}
	for input, want := range tests {
		if got := normalizeTerminalLine(input); got != want {
			t.Errorf("normalizeTerminalLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPrestartExtractionStopsWhenTheAgentStarts(t *testing.T) {
	logDir := t.TempDir()
	splitter := hooks.NewSplitter(hooks.MarkerKindPrestart, hooks.LogKindPrestart, logDir, []string{"001-a", "002-b"})
	lines := []string{
		"::hole-hook-begin prestart 001-a::\r",
		// Printed by the script itself, so it is output, not the entrypoint's line.
		agentStartLine + "\r",
		"::hole-hook-end prestart 001-a exit=0::\r",
		"::hole-hook-begin prestart 002-b::\r",
		"\x1b[32mb-output\x1b[0m\r",
		"::hole-hook-end prestart 002-b exit=0::\r",
		agentStartLine + "\r",
		"agent-output\r",
		"::hole-hook-end prestart 002-b exit=9::\r",
	}
	consumed := 0
	for _, line := range lines {
		if !consumePrestartLine(splitter, line) {
			break
		}
		consumed++
	}
	splitter.Close("")

	if consumed != 6 {
		t.Errorf("consumed %d lines, want reading to stop at the entrypoint's %q", consumed, agentStartLine)
	}
	results := splitter.Results()
	if len(results) != 2 || results[1].ExitCode != 0 {
		t.Fatalf("the agent must not be able to rewrite a prestart exit code: %+v", results)
	}
	a := fileContent(t, filepath.Join(logDir, "prestart-001-a.log"))
	if !strings.Contains(a, "\n"+agentStartLine+"\n") {
		t.Errorf("a script's own output was cut:\n%s", a)
	}
	b := fileContent(t, filepath.Join(logDir, "prestart-002-b.log"))
	if !strings.Contains(b, "\nb-output\n") || strings.Contains(b, "agent-output") {
		t.Errorf("002-b log:\n%s", b)
	}
}

func TestPrestartScriptNames(t *testing.T) {
	runTmpDir := t.TempDir()
	dir := filepath.Join(runTmpDir, "prestart-scripts")
	for _, name := range []string{"002-b", "001-a", ".gitkeep"} {
		writeScript(t, filepath.Join(dir, name), "")
	}
	if names := prestartScriptNames(runTmpDir); strings.Join(names, " ") != "001-a 002-b" {
		t.Errorf("names = %v", names)
	}
	if names := prestartScriptNames(""); names != nil {
		t.Errorf("names = %v", names)
	}
}
