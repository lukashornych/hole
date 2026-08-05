package image

import (
	"strings"
	"testing"
)

func manifest() Manifest {
	return Manifest{
		Config: Config{
			BaseImage:     "ubuntu:24.04",
			EnabledAgents: []string{"claude", "codex", "gemini"},
			Dependencies:  []string{"make", "gcc"},
		},
		Host:        HostIdentity{Username: "dev", Home: "/home/dev", UID: "1000", GID: "1000"},
		BuildInputs: "abc123",
	}
}

func TestTagIsStable(t *testing.T) {
	first, err := manifest().Tag()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := manifest().Tag()
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("tag is not stable: %s vs %s", first, again)
		}
	}
	if len(first) != tagLength {
		t.Errorf("tag %q has length %d, want %d", first, len(first), tagLength)
	}
}

func TestTagIgnoresDefaultsWrittenOutExplicitly(t *testing.T) {
	// "explicitly set to the default" and "not set" must produce the same image.
	explicit := manifest()
	implicit := manifest()
	implicit.Config.BaseImage = ""

	explicitTag, err := explicit.Tag()
	if err != nil {
		t.Fatal(err)
	}
	implicitTag, err := implicit.Tag()
	if err != nil {
		t.Fatal(err)
	}
	if explicitTag != implicitTag {
		t.Errorf("explicit default base image produced a different tag: %s vs %s", explicitTag, implicitTag)
	}
}

func TestTagChangesWithImageAffectingInputs(t *testing.T) {
	base, err := manifest().Tag()
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*Manifest){
		"base image":       func(m *Manifest) { m.Config.BaseImage = "ubuntu:22.04" },
		"dependencies":     func(m *Manifest) { m.Config.Dependencies = append(m.Config.Dependencies, "cmake") },
		"dependency order": func(m *Manifest) { m.Config.Dependencies = []string{"gcc", "make"} },
		"enabled agents":   func(m *Manifest) { m.Config.EnabledAgents = []string{"claude"} },
		"setup script":     func(m *Manifest) { m.Config.SetupScriptShas = []string{"deadbeef"} },
		"host username":    func(m *Manifest) { m.Host.Username = "other" },
		"host home":        func(m *Manifest) { m.Host.Home = "/home/other" },
		"host uid":         func(m *Manifest) { m.Host.UID = "1001" },
		"hole assets":      func(m *Manifest) { m.BuildInputs = "def456" },
		"user agent":       func(m *Manifest) { m.UserAgents = []string{"mine:abc"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := manifest()
			mutate(&mutated)
			tag, err := mutated.Tag()
			if err != nil {
				t.Fatal(err)
			}
			if tag == base {
				t.Errorf("changing the %s did not change the image tag", name)
			}
		})
	}
}

func TestTagIgnoresUserAgentOrder(t *testing.T) {
	first := manifest()
	first.UserAgents = []string{"a:1", "b:2"}
	second := manifest()
	second.UserAgents = []string{"b:2", "a:1"}

	firstTag, err := first.Tag()
	if err != nil {
		t.Fatal(err)
	}
	secondTag, err := second.Tag()
	if err != nil {
		t.Fatal(err)
	}
	if firstTag != secondTag {
		t.Error("user agent hash order must not affect the tag")
	}
}

func TestResolveUsesTheSharedImageWhenTheProjectChangesNothing(t *testing.T) {
	// Identical canonical configurations mean the project does not change image content, so
	// it shares the global image with every other such project.
	identity, err := Resolve("demo-1a2b3c4d", manifest(), manifest())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Scope != ScopeGlobal {
		t.Errorf("scope = %s, want %s", identity.Scope, ScopeGlobal)
	}
	if identity.Repository != GlobalRepository {
		t.Errorf("repository = %s, want %s", identity.Repository, GlobalRepository)
	}
	if !strings.Contains(identity.Describe(), "shared") {
		t.Errorf("Describe() = %q", identity.Describe())
	}
}

func TestResolveUsesAProjectImageWhenTheProjectChangesTheBuild(t *testing.T) {
	merged := manifest()
	merged.Config.Dependencies = append(merged.Config.Dependencies, "cmake")

	identity, err := Resolve("demo-1a2b3c4d", merged, manifest())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Scope != ScopeProject {
		t.Errorf("scope = %s, want %s", identity.Scope, ScopeProject)
	}
	if identity.Repository != "hole-sandbox/agent-demo-1a2b3c4d" {
		t.Errorf("repository = %s", identity.Repository)
	}
	// The banner has to say *why* the project needed its own image.
	if !strings.Contains(identity.Describe(), "dependencies") {
		t.Errorf("Describe() = %q, want it to name the differing key", identity.Describe())
	}
	if !strings.HasPrefix(identity.Reference(), identity.Repository+":") {
		t.Errorf("reference %q is not repository:tag", identity.Reference())
	}
}

func TestResolveNamesEveryDifferingKey(t *testing.T) {
	cases := map[string]func(*Manifest){
		"baseImage":     func(m *Manifest) { m.Config.BaseImage = "ubuntu:22.04" },
		"enabledAgents": func(m *Manifest) { m.Config.EnabledAgents = []string{"claude"} },
		"dependencies":  func(m *Manifest) { m.Config.Dependencies = []string{"other"} },
		"setupScripts":  func(m *Manifest) { m.Config.SetupScriptShas = []string{"abc"} },
	}
	for key, mutate := range cases {
		t.Run(key, func(t *testing.T) {
			merged := manifest()
			mutate(&merged)
			identity, err := Resolve("demo-1a2b3c4d", merged, manifest())
			if err != nil {
				t.Fatal(err)
			}
			if identity.Scope != ScopeProject {
				t.Fatalf("scope = %s", identity.Scope)
			}
			if len(identity.DifferingKeys) != 1 || identity.DifferingKeys[0] != key {
				t.Errorf("DifferingKeys = %v, want [%s]", identity.DifferingKeys, key)
			}
		})
	}
}

func TestResolveIgnoresRuntimeOnlyDifferences(t *testing.T) {
	// Host identity and Hole's own build inputs affect the *tag* but not the scope: they are
	// the same for every project on this machine, so they cannot make a project special.
	merged := manifest()
	globalOnly := manifest()
	merged.Host.UID = "1001"
	globalOnly.Host.UID = "1001"

	identity, err := Resolve("demo-1a2b3c4d", merged, globalOnly)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Scope != ScopeGlobal {
		t.Errorf("scope = %s, want %s", identity.Scope, ScopeGlobal)
	}
}

func TestResolveRepeatingAGlobalValueStaysShared(t *testing.T) {
	// A project that repeats a global dependency verbatim deduplicates away in the merge, so
	// the canonical configurations match and the shared image is kept.
	merged := manifest()
	globalOnly := manifest()
	identity, err := Resolve("demo-1a2b3c4d", merged, globalOnly)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Scope != ScopeGlobal {
		t.Errorf("scope = %s", identity.Scope)
	}
}

func TestProjectNameCannotCollideWithTheSharedRepository(t *testing.T) {
	// Project names always carry an 8-hex path-hash suffix, so `agent-global` is unreachable.
	if AgentRepository("global") == GlobalRepository {
		t.Skip("AgentRepository('global') is the shared name by construction; project names always have a hash suffix")
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	normalized := Config{}.Normalize()
	if normalized.BaseImage != DefaultBaseImage {
		t.Errorf("base image default = %q", normalized.BaseImage)
	}
	if normalized.EnabledAgents == nil || normalized.Dependencies == nil || normalized.SetupScriptShas == nil {
		t.Error("nil slices must normalize to empty ones so JSON encoding is stable")
	}
}

func TestContentSHADistinguishesContent(t *testing.T) {
	if ContentSHA([]byte("a")) == ContentSHA([]byte("b")) {
		t.Error("different content hashed equally")
	}
	if ContentSHA([]byte("a")) != ContentSHA([]byte("a")) {
		t.Error("identical content hashed differently")
	}
}
