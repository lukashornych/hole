package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lukashornych/hole/v2/internal/network"
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
		// Builtins are offered for in-place updates, which needs both halves: a script that
		// updates the CLI and a way to tell whether it did.
		updates, err := agent.UpdateScripts()
		if err != nil || len(updates) == 0 {
			t.Errorf("agent %q has no update script: %v", name, err)
		}
		spec, err := agent.VersionSpec()
		if err != nil || spec == nil {
			t.Errorf("agent %q has no valid version.json: %v", name, err)
		}
	}
}

// Update scripts must never reach the main image build, whose COPY of agent-installs/ takes
// every file InstallScripts returns.
func TestInstallScriptsExcludeUpdateScripts(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := registry.Get("claude")
	scripts, err := agent.InstallScripts()
	if err != nil {
		t.Fatal(err)
	}
	for name := range scripts {
		if strings.HasPrefix(name, "update-") {
			t.Errorf("InstallScripts returned %s", name)
		}
	}
}

// Every enabled agent's allow.txt is merged into every sandbox's policy, so a builtin entry that
// wildcards a namespace shared with other tenants opens attacker-controlled endpoints in the
// default configuration — `*.googleapis.com` covers `storage.googleapis.com`, where anyone can
// create a writable bucket. The gateway matches names, not URL paths, so it cannot tell those apart
// from the vendor's own API. Wildcards over a namespace one vendor owns (`*.chatgpt.com`) are fine.
func TestBuiltinAllowListsDoNotWildcardMultiTenantNamespaces(t *testing.T) {
	multiTenant := []string{"googleapis.com", "amazonaws.com", "azurewebsites.net", "cloudfront.net",
		"blob.core.windows.net", "r2.cloudflarestorage.com", "herokuapp.com", "web.app",
		"firebaseio.com", "pages.dev", "workers.dev", "vercel.app", "netlify.app", "github.io"}

	registry, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"claude", "gemini", "codex"} {
		agent, ok := registry.Get(name)
		if !ok {
			t.Fatalf("builtin agent %q missing from the registry", name)
		}
		allow, err := agent.AllowFile()
		if err != nil {
			t.Fatalf("agent %q allow.txt: %v", name, err)
		}
		entries, err := network.ParseAllowFile(allow, name)
		if err != nil {
			t.Fatalf("agent %q allow.txt does not parse: %v", name, err)
		}
		for _, entry := range entries {
			if entry.Kind != network.KindWildcard {
				continue
			}
			for _, namespace := range multiTenant {
				if entry.Host == namespace || strings.HasSuffix(entry.Host, "."+namespace) {
					t.Errorf("agent %q allows '%s': %s is multi-tenant, so the wildcard also allows "+
						"other tenants' endpoints; name the specific hosts the CLI needs instead",
						name, entry, namespace)
				}
			}
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
		// version.json runs the CLI with the same interpreter, so it carries the pin as well.
		spec, err := agent.VersionSpec()
		if err != nil {
			t.Fatalf("agent %q version.json: %v", name, err)
		}
		if spec != nil {
			command = append(command, spec.Command...)
		}
		// command-bare.json bakes a wrapper running the CLI under the same interpreter.
		bare, err := agent.BareCommand()
		if err != nil {
			t.Fatalf("agent %q command-bare.json: %v", name, err)
		}
		command = append(command, bare...)
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
			t.Errorf("agent %q references more than one Node version in command.json, version.json and command-bare.json: %v", name, pinned)
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
				t.Errorf("agent %q pins Node v%s but no install script runs "+
					"`nvm install %s`; the agent would fail to launch with ENOENT", name, version, version)
			}
		}

		// nvm's default may be another Node (a setup hook installing a newer one makes it the
		// default), so an update script has to run the pinned Node's npm by absolute path, or it
		// updates the CLI of a Node the agent never runs and the update is offered forever.
		updates, err := agent.UpdateScripts()
		if err != nil {
			t.Fatalf("agent %q update scripts: %v", name, err)
		}
		for file, body := range updates {
			for version := range pinned {
				if !strings.Contains(string(body), "/.nvm/versions/node/v"+version+"/bin") {
					t.Errorf("agent %q %s does not run the pinned Node v%s by absolute path; "+
						"it would update the CLI under nvm's default Node", name, file, version)
				}
			}
		}
	}
}

// codex and gemini run under a pinned nvm Node that is not on PATH, so only the wrapper makes
// them callable by name; claude's native installer already puts it into ~/.local/bin, where a
// wrapper would collide and fail the build.
func TestBuiltinBareCommands(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, want := range map[string]bool{"claude": false, "codex": true, "gemini": true} {
		agent, ok := registry.Get(name)
		if !ok {
			t.Fatalf("builtin agent %q missing from the registry", name)
		}
		bare, err := agent.BareCommand()
		if err != nil {
			t.Fatalf("agent %q: %v", name, err)
		}
		if (bare != nil) != want {
			t.Errorf("agent %q has command-bare.json = %v, want %v", name, bare != nil, want)
		}
	}
}

func TestBareCommand(t *testing.T) {
	for _, test := range []struct {
		name    string
		files   fstest.MapFS
		want    []string
		wantErr bool
	}{
		{"missing file", fstest.MapFS{}, nil, false},
		{"valid", fstest.MapFS{"command-bare.json": {Data: []byte(`["$HOME/node", "$HOME/cli"]`)}}, []string{"$HOME/node", "$HOME/cli"}, false},
		{"invalid JSON", fstest.MapFS{"command-bare.json": {Data: []byte(`["a",`)}}, nil, true},
		{"not an array of strings", fstest.MapFS{"command-bare.json": {Data: []byte(`{"command": ["a"]}`)}}, nil, true},
		{"empty array", fstest.MapFS{"command-bare.json": {Data: []byte(`[]`)}}, nil, true},
		{"empty item", fstest.MapFS{"command-bare.json": {Data: []byte(`["/a", ""]`)}}, nil, true},
		{"bare first part", fstest.MapFS{"command-bare.json": {Data: []byte(`["demo", "--flag"]`)}}, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := &Agent{Name: "demo", Source: SourceUser, Dir: test.files}
			got, err := agent.BareCommand()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "command-bare.json") || !strings.Contains(err.Error(), "demo") {
					t.Errorf("error %v, want one naming the agent and command-bare.json", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BareCommand: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// Claude Code no longer honours a trust entry above the git root, so the baked `/` entry cannot
// stop its trust dialog; the sandbox flag must reach both the exec'd agent and shells.
func TestClaudeRunsAsSandboxed(t *testing.T) {
	registry, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent, ok := registry.Get("claude")
	if !ok {
		t.Fatal("builtin agent claude missing from the registry")
	}
	command, err := agent.Command()
	if err != nil {
		t.Fatalf("claude command: %v", err)
	}
	if !slices.Contains(command, "CLAUDE_CODE_SANDBOXED=1") {
		t.Errorf("claude command %q does not set CLAUDE_CODE_SANDBOXED=1", command)
	}
	scripts, err := agent.InstallScripts()
	if err != nil {
		t.Fatalf("claude install scripts: %v", err)
	}
	if !strings.Contains(string(scripts["install-user.sh"]), "export CLAUDE_CODE_SANDBOXED=1") {
		t.Errorf("claude install-user.sh does not export CLAUDE_CODE_SANDBOXED=1 for shells")
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

func TestVersionSpecValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		valid   bool
	}{
		{"npm source", `{"command": ["x", "--version"], "latest": {"npm": "@scope/pkg"}}`, true},
		{"url source", `{"command": ["x"], "latest": {"url": "https://example.com/latest"}}`, true},
		{"npm path traversal", `{"command": ["x"], "latest": {"npm": "../../evil?x=1"}}`, false},
		{"both sources", `{"command": ["x"], "latest": {"npm": "pkg", "url": "https://example.com"}}`, false},
		{"no source", `{"command": ["x"], "latest": {}}`, false},
		{"empty command", `{"command": [], "latest": {"npm": "pkg"}}`, false},
		{"missing command", `{"latest": {"npm": "pkg"}}`, false},
		{"not a URL", `{"command": ["x"], "latest": {"url": "downloads/latest"}}`, false},
		{"file URL", `{"command": ["x"], "latest": {"url": "file:///etc/passwd"}}`, false},
		{"unknown key", `{"command": ["x"], "latest": {"npm": "pkg"}, "extra": 1}`, false},
		{"not JSON", `command: x`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			userAgent(t, root, "versioned")
			if err := os.WriteFile(filepath.Join(root, "versioned", "version.json"), []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			registry, err := Load(root)
			if err != nil {
				t.Fatalf("a malformed version.json must not fail the registry load: %v", err)
			}
			agent, _ := registry.Get("versioned")
			spec, err := agent.VersionSpec()
			if test.valid && (err != nil || spec == nil) {
				t.Errorf("VersionSpec = %v, %v; want a spec", spec, err)
			}
			if !test.valid && err == nil {
				t.Errorf("VersionSpec accepted %s", test.content)
			}
		})
	}
}

func TestUserAgentWithoutUpdateFiles(t *testing.T) {
	root := t.TempDir()
	userAgent(t, root, "plain")
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := registry.Get("plain")
	if spec, err := agent.VersionSpec(); spec != nil || err != nil {
		t.Errorf("VersionSpec = %v, %v; want none", spec, err)
	}
	if scripts, err := agent.UpdateScripts(); len(scripts) != 0 || err != nil {
		t.Errorf("UpdateScripts = %v, %v; want none", scripts, err)
	}
}

// An enabled agent has both update files or neither; a disabled agent is never checked.
func TestResolveEnabledChecksUpdateFiles(t *testing.T) {
	const version = `{"command": ["x", "--version"], "latest": {"npm": "pkg"}}`
	tests := []struct {
		name  string
		files map[string]string
		valid bool
	}{
		{"no update files", nil, true},
		{"user script and version.json", map[string]string{"update-user.sh": "exit 0\n", "version.json": version}, true},
		{"root script and version.json", map[string]string{"update-root.sh": "exit 0\n", "version.json": version}, true},
		{"update script only", map[string]string{"update-user.sh": "exit 0\n"}, false},
		{"version.json only", map[string]string{"version.json": version}, false},
		{"malformed version.json", map[string]string{"update-user.sh": "exit 0\n", "version.json": `{}`}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			userAgent(t, root, "custom")
			for file, content := range test.files {
				if err := os.WriteFile(filepath.Join(root, "custom", file), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			registry, err := Load(root)
			if err != nil {
				t.Fatalf("update files must not fail the registry load: %v", err)
			}
			_, err = registry.ResolveEnabled([]string{"claude", "custom"})
			if test.valid && err != nil {
				t.Errorf("ResolveEnabled: %v", err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), "custom")) {
				t.Errorf("ResolveEnabled error = %v, want one naming the agent", err)
			}
			if _, err := registry.ResolveEnabled([]string{"claude"}); err != nil {
				t.Errorf("a disabled agent was checked: %v", err)
			}
		})
	}
}
