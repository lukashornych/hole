package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/hostenv"
)

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveKeepsOrderAndSkipsMissing(t *testing.T) {
	projectDir := t.TempDir()
	script(t, projectDir, ".hole/first.sh", "#!/bin/bash\nexit 0\n")
	script(t, projectDir, ".hole/second.sh", "#!/bin/bash\nexit 0\n")

	entries := []config.ScriptEntry{
		{Script: ".hole/first.sh"},
		{Script: ".hole/missing.sh"},
		{Script: ".hole/second.sh"},
	}
	resolved := Resolve(entries, hostenv.Host{Home: "/home/dev"}, projectDir, "prestart")
	if len(resolved) != 2 {
		t.Fatalf("expected the missing script to be skipped, got %d scripts", len(resolved))
	}
	if resolved[0].Name() != "first.sh" || resolved[1].Name() != "second.sh" {
		t.Errorf("hook order changed: %s, %s", resolved[0].Name(), resolved[1].Name())
	}
}

func TestResolveSkipsDirectories(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved := Resolve([]config.ScriptEntry{{Script: "scripts"}}, hostenv.Host{}, projectDir, "setup"); len(resolved) != 0 {
		t.Errorf("a directory must not be accepted as a hook script: %v", resolved)
	}
}

func TestResolveExpandsPathPipeline(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	script(t, home, "hooks/global.sh", "#!/bin/bash\nexit 0\n")
	t.Setenv("HOLE_HOOK_DIR", filepath.Join(home, "hooks"))

	host := hostenv.Host{Home: home}
	resolved := Resolve([]config.ScriptEntry{
		{Script: "~/hooks/global.sh"},
		{Script: "${HOLE_HOOK_DIR}/global.sh"},
	}, host, projectDir, "setupHost")
	if len(resolved) != 2 {
		t.Fatalf("path pipeline did not resolve both forms: %v", resolved)
	}
}

func TestResolveHandlesAbsentAndEmptyEntries(t *testing.T) {
	if resolved := Resolve(nil, hostenv.Host{}, t.TempDir(), "setup"); resolved != nil {
		t.Errorf("no entries resolved to %v", resolved)
	}
	if resolved := Resolve([]config.ScriptEntry{{Script: ""}}, hostenv.Host{}, t.TempDir(), "setup"); resolved != nil {
		t.Errorf("an empty entry resolved to %v", resolved)
	}
}

func TestRunSetupHostFailureAborts(t *testing.T) {
	dir := t.TempDir()
	failing := Script{Path: script(t, dir, "fail.sh", "#!/bin/bash\nexit 3\n")}
	if err := RunSetupHost([]Script{failing}, nil); err == nil {
		t.Error("a failing setupHost hook must abort startup")
	}
}

func TestRunSetupHostPassesEnvironment(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	checking := Script{Path: script(t, dir, "check.sh",
		"#!/bin/bash\n[[ \"${HOLE_PROJECT_NAME}\" == \"demo\" ]] && touch \""+marker+"\"\n")}
	if err := RunSetupHost([]Script{checking}, []string{"HOLE_PROJECT_NAME=demo"}); err != nil {
		t.Fatalf("RunSetupHost: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("hook did not see Hole's exported environment")
	}
}

func TestRunCleanupHostContinuesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-ran")
	scripts := []Script{
		{Path: script(t, dir, "one.sh", "#!/bin/bash\nexit 1\n")},
		{Path: script(t, dir, "two.sh", "#!/bin/bash\ntouch \""+marker+"\"\n")},
	}
	// Teardown never aborts: a failing cleanup hook must not stop the remaining ones.
	RunCleanupHost(scripts, nil)
	if _, err := os.Stat(marker); err != nil {
		t.Error("teardown stopped after a failing cleanupHost hook")
	}
}

func TestScriptContent(t *testing.T) {
	dir := t.TempDir()
	body := "#!/bin/bash\necho hi\n"
	s := Script{Path: script(t, dir, "s.sh", body)}
	content, err := s.Content()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != body {
		t.Errorf("content = %q", content)
	}
}

func TestResolveExpandsGlobsInLexicographicOrder(t *testing.T) {
	projectDir := t.TempDir()
	// Written out of order on purpose: the run order must come from the names, not the
	// filesystem, so `NNN-` prefixes give a predictable sequence.
	for _, name := range []string{"setup.d/030-c.sh", "setup.d/010-a.sh", "setup.d/020-b.sh"} {
		script(t, projectDir, name, "#!/bin/bash\nexit 0\n")
	}
	// A non-matching file must be left out.
	script(t, projectDir, "setup.d/notes.md", "not a script\n")

	resolved := Resolve([]config.ScriptEntry{{Script: "setup.d/*.sh"}}, hostenv.Host{}, projectDir, "setup")
	if len(resolved) != 3 {
		t.Fatalf("expected 3 scripts, got %d: %v", len(resolved), resolved)
	}
	for i, want := range []string{"010-a.sh", "020-b.sh", "030-c.sh"} {
		if resolved[i].Name() != want {
			t.Errorf("script %d = %s, want %s", i, resolved[i].Name(), want)
		}
	}
}

func TestResolveGlobKeepsEntryOrderAroundMatches(t *testing.T) {
	projectDir := t.TempDir()
	script(t, projectDir, "first.sh", "#!/bin/bash\nexit 0\n")
	script(t, projectDir, "setup.d/010-a.sh", "#!/bin/bash\nexit 0\n")
	script(t, projectDir, "last.sh", "#!/bin/bash\nexit 0\n")

	resolved := Resolve([]config.ScriptEntry{
		{Script: "first.sh"},
		{Script: "setup.d/*.sh"},
		{Script: "last.sh"},
	}, hostenv.Host{}, projectDir, "setup")

	var names []string
	for _, s := range resolved {
		names = append(names, s.Name())
	}
	if len(names) != 3 || names[0] != "first.sh" || names[1] != "010-a.sh" || names[2] != "last.sh" {
		t.Errorf("matches must take the pattern's place in entry order, got %v", names)
	}
}

func TestResolveGlobstarPattern(t *testing.T) {
	projectDir := t.TempDir()
	script(t, projectDir, "hooks/a/deep.sh", "#!/bin/bash\nexit 0\n")
	script(t, projectDir, "hooks/b/c/deeper.sh", "#!/bin/bash\nexit 0\n")

	resolved := Resolve([]config.ScriptEntry{{Script: "hooks/**/*.sh"}}, hostenv.Host{}, projectDir, "prestart")
	if len(resolved) != 2 {
		t.Errorf("globstar pattern matched %d scripts, want 2: %v", len(resolved), resolved)
	}
}

func TestResolveGlobWithNoMatchesIsSkipped(t *testing.T) {
	projectDir := t.TempDir()
	// A pattern matching nothing is a user configuration problem, not a fatal one.
	if resolved := Resolve([]config.ScriptEntry{{Script: "setup.d/*.sh"}}, hostenv.Host{}, projectDir, "setup"); len(resolved) != 0 {
		t.Errorf("expected no scripts, got %v", resolved)
	}
}

func TestResolveGlobSkipsDirectories(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, "hooks", "a.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	script(t, projectDir, "hooks/b.sh", "#!/bin/bash\nexit 0\n")

	resolved := Resolve([]config.ScriptEntry{{Script: "hooks/*.sh"}}, hostenv.Host{}, projectDir, "setup")
	if len(resolved) != 1 || resolved[0].Name() != "b.sh" {
		t.Errorf("a directory matching the pattern must be skipped, got %v", resolved)
	}
}
