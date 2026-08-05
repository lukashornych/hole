package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/lukashornych/hole/internal/network"
)

func TestBuiltinAgentsAreComplete(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"claude", "gemini", "codex"} {
		agent, ok := registry.Get(name)
		if !ok {
			t.Fatalf("builtin agent %q missing from the registry", name)
		}
		if agent.Source != SourceBuiltin {
			t.Errorf("agent %q has source %q", name, agent.Source)
		}
		command, err := agent.Command()
		if err != nil {
			t.Errorf("agent %q command: %v", name, err)
		}
		if len(command) == 0 {
			t.Errorf("agent %q has an empty command", name)
		}
		allow, err := agent.AllowFile()
		if err != nil {
			t.Errorf("agent %q allow.txt: %v", name, err)
		}
		entries, err := network.ParseAllowFile(allow, name)
		if err != nil {
			t.Errorf("agent %q allow.txt does not parse: %v", name, err)
		}
		if len(entries) == 0 {
			t.Errorf("agent %q allows no domains; its CLI could not reach its API", name)
		}
		scripts, err := agent.InstallScripts()
		if err != nil {
			t.Errorf("agent %q install scripts: %v", name, err)
		}
		if len(scripts) == 0 {
			t.Errorf("agent %q has no install script, so its CLI would not be installed", name)
		}
	}
}

// An agent whose command names the Node interpreter by absolute path pins a patch version, and
// its install script must install exactly that version. The two files drifting apart is invisible
// until launch, where it surfaces as ENOENT inside the sandbox — and it drifts on its own, because
// `nvm install 22` follows whatever the newest 22.x is at build time.
func TestPinnedNodeVersionMatchesTheInstallScript(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nodePath := regexp.MustCompile(`/\.nvm/versions/node/v([0-9]+\.[0-9]+\.[0-9]+)/`)

	for _, name := range registry.Names() {
		agent, _ := registry.Get(name)
		command, err := agent.Command()
		if err != nil {
			t.Fatalf("agent %q command: %v", name, err)
		}
		pinned := map[string]bool{}
		for _, part := range command {
			if match := nodePath.FindStringSubmatch(part); match != nil {
				pinned[match[1]] = true
			}
		}
		if len(pinned) == 0 {
			continue
		}
		if len(pinned) > 1 {
			t.Errorf("agent %q references more than one Node version in its command: %v", name, pinned)
		}

		scripts, err := agent.InstallScripts()
		if err != nil {
			t.Fatalf("agent %q install scripts: %v", name, err)
		}
		for version := range pinned {
			found := false
			for _, body := range scripts {
				if strings.Contains(string(body), "nvm install "+version) {
					found = true
				}
			}
			if !found {
				t.Errorf("agent %q pins Node v%s in command.json but no install script runs "+
					"`nvm install %s`; the agent would fail to launch with ENOENT", name, version, version)
			}
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"claude", "codex", "gemini"}) {
		t.Errorf("Names() = %v", got)
	}
}

func TestResolveRejectsUnknownAndMalformed(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "nope", "Claude", "claude:profile", "claude/x", "-claude"} {
		if _, err := registry.Resolve(name); err == nil {
			t.Errorf("Resolve(%q) succeeded", name)
		}
	}
	if _, err := registry.Resolve("claude"); err != nil {
		t.Errorf("Resolve(claude): %v", err)
	}
}

func TestResolveEnabledDefaultsToEveryAgent(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(EnabledNames(enabled), []string{"claude", "codex", "gemini"}) {
		t.Errorf("default enabled agents = %v", EnabledNames(enabled))
	}
}

func TestResolveEnabledHonorsConfiguredOrderAndDeduplicates(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := registry.ResolveEnabled([]string{"codex", "claude", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(EnabledNames(enabled), []string{"codex", "claude"}) {
		t.Errorf("enabled agents = %v, want [codex claude]", EnabledNames(enabled))
	}
}

func TestResolveEnabledRejectsUnknownAgent(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveEnabled([]string{"nope"}); err == nil {
		t.Error("unknown agent in container.enabledAgents was accepted")
	}
}

// userAgent writes a minimal user agent plugin, which is also how the e2e suite gets an
// agent that needs no API key.
func userAgent(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"command.json":    `["bash", "-c", "while :; do sleep 1; done"]`,
		"allow.txt":       "example.com\n",
		"install-user.sh": "#!/bin/bash\nexit 0\n",
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadDiscoversUserAgents(t *testing.T) {
	root := t.TempDir()
	userAgent(t, root, "test-agent")

	registry, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, ok := registry.Get("test-agent")
	if !ok {
		t.Fatal("user agent was not discovered")
	}
	if agent.Source != SourceUser {
		t.Errorf("source = %q, want %q", agent.Source, SourceUser)
	}
	command, err := agent.Command()
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if command[0] != "bash" {
		t.Errorf("command = %v", command)
	}
	// A user agent participates in enabled-agent resolution like any builtin.
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(EnabledNames(enabled), []string{"claude", "codex", "gemini", "test-agent"}) {
		t.Errorf("enabled agents = %v", EnabledNames(enabled))
	}
}

func TestLoadRejectsUserAgentShadowingBuiltin(t *testing.T) {
	root := t.TempDir()
	userAgent(t, root, "claude")
	if _, err := Load(root); err == nil {
		t.Error("a user agent shadowing a builtin must be fatal")
	}
}

func TestLoadRejectsInvalidUserAgentName(t *testing.T) {
	root := t.TempDir()
	userAgent(t, root, "Bad_Name")
	if _, err := Load(root); err == nil {
		t.Error("an invalid user agent directory name must be fatal")
	}
}

func TestLoadIgnoresMissingUserAgentsDir(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("a missing user agents directory must be fine: %v", err)
	}
}

func TestUserAgentWithoutCommandFailsWhenUsed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, ok := registry.Get("broken")
	if !ok {
		t.Fatal("agent not registered")
	}
	if _, err := agent.Command(); err == nil {
		t.Error("an agent without command.json must fail when it is started")
	}
}
