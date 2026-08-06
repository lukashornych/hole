package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// merged resolves a profile across two documents and returns the typed settings, which is
// what every consumer downstream actually sees.
func merged(t *testing.T, global, project, profile string) *Settings {
	t.Helper()
	globalDoc := doc(t, global)
	projectDoc := doc(t, project)

	var document Document
	if profile == "" {
		// Mirrors loadSettingsDocument: an empty chain is the plain two-way merge, and going
		// through MergeWithProfile is what keeps argument vectors out of the array dedup.
		document = MergeWithProfile(globalDoc, projectDoc, nil)
	} else {
		chain, err := ResolveChain(globalDoc, projectDoc, profile)
		if err != nil {
			t.Fatalf("ResolveChain(%q): %v", profile, err)
		}
		document = MergeWithProfile(globalDoc, projectDoc, chain)
	}

	if _, leaked := document[profilesKey]; leaked {
		t.Error("the merged document still contains the profiles key")
	}
	if _, leaked := document[extendsKey]; leaked {
		t.Error("the merged document still contains the extends key")
	}

	settings, err := Decode(document)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return settings
}

func TestValidateProfileName(t *testing.T) {
	for _, name := range []string{"research", "a", "docker-2", "research-docker", "x1"} {
		if err := ValidateProfileName(name); err != nil {
			t.Errorf("valid name %q rejected: %v", name, err)
		}
	}
	// `:` is the CLI separator and `,` is reserved for a future multi-profile syntax.
	for _, name := range []string{"", "Foo", "a:b", "a,b", "-lead", "with space", "under_score"} {
		if err := ValidateProfileName(name); err == nil {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}

func TestNoProfileDegeneratesToTheTwoWayMerge(t *testing.T) {
	settings := merged(t,
		`{"network":{"allow":["global.example.com"]},"profiles":{"research":{"network":{"allow":["research.example.com"]}}}}`,
		`{"network":{"allow":["project.example.com"]}}`,
		"")
	want := []string{"global.example.com", "project.example.com"}
	if !reflect.DeepEqual(settings.Network.Allow, want) {
		t.Errorf("allow = %v, want %v — an unselected profile must contribute nothing", settings.Network.Allow, want)
	}
}

func TestProfileInBothFilesMergesFourWays(t *testing.T) {
	settings := merged(t,
		`{"dependencies":["global-base"],"profiles":{"p":{"dependencies":["global-overlay"]}}}`,
		`{"dependencies":["project-base"],"profiles":{"p":{"dependencies":["project-overlay"]}}}`,
		"p")
	// Order is global base → global overlay → project base → project overlay.
	want := []string{"global-base", "global-overlay", "project-base", "project-overlay"}
	if !reflect.DeepEqual(settings.Dependencies, want) {
		t.Errorf("dependencies = %v, want %v", settings.Dependencies, want)
	}
}

func TestProjectStillBeatsGlobalOnScalars(t *testing.T) {
	settings := merged(t,
		`{"container":{"memoryLimit":"2g"},"profiles":{"p":{"container":{"memoryLimit":"4g"}}}}`,
		`{"container":{"memoryLimit":"8g"},"profiles":{"p":{"container":{"memoryLimit":"16g"}}}}`,
		"p")
	if settings.Container.MemoryLimit != "16g" {
		t.Errorf("memoryLimit = %q, want 16g (the project profile is the highest-precedence overlay)", settings.Container.MemoryLimit)
	}
}

func TestProfileDefinedInOnlyOneFile(t *testing.T) {
	settings := merged(t,
		`{"profiles":{"p":{"dependencies":["from-global"]}}}`,
		`{"dependencies":["project-base"]}`,
		"p")
	if !reflect.DeepEqual(settings.Dependencies, []string{"from-global", "project-base"}) {
		t.Errorf("dependencies = %v", settings.Dependencies)
	}
}

func TestUnknownProfileIsFatalAndListsWhatExists(t *testing.T) {
	_, err := ResolveChain(
		doc(t, `{"profiles":{"research":{}}}`),
		doc(t, `{"profiles":{"docker":{}}}`),
		"nope")
	if err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	var unknown *UnknownProfileError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected an UnknownProfileError, got %T", err)
	}
	message := err.Error()
	for _, want := range []string{"nope", "research", "docker"} {
		if !strings.Contains(message, want) {
			t.Errorf("error should mention %q:\n%s", want, message)
		}
	}
}

func TestProfileRequestedWithNoSettingsFilesAtAll(t *testing.T) {
	if _, err := ResolveChain(nil, nil, "research"); err == nil {
		t.Error("a profile must be fatal when no settings file defines it")
	}
}

func TestExtendsChainOrder(t *testing.T) {
	document := doc(t, `{
	  "profiles": {
	    "research": {"dependencies": ["research"]},
	    "docker": {"dependencies": ["docker"]},
	    "research-docker": {"extends": ["research", "docker"], "dependencies": ["leaf"]}
	  }
	}`)
	chain, err := ResolveChain(document, nil, "research-docker")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	// Parents before children, in listed order, leaf last.
	if !reflect.DeepEqual(chain, []string{"research", "docker", "research-docker"}) {
		t.Errorf("chain = %v", chain)
	}

	settings := merged(t, mustJSON(t, document), `{}`, "research-docker")
	if !reflect.DeepEqual(settings.Dependencies, []string{"research", "docker", "leaf"}) {
		t.Errorf("dependencies = %v", settings.Dependencies)
	}
}

func TestExtendsAcceptsStringForm(t *testing.T) {
	chain, err := ResolveChain(doc(t, `{"profiles":{"a":{},"b":{"extends":"a"}}}`), nil, "b")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if !reflect.DeepEqual(chain, []string{"a", "b"}) {
		t.Errorf("chain = %v", chain)
	}
}

func TestExtendsDiamondAppliesEachProfileOnce(t *testing.T) {
	document := doc(t, `{
	  "profiles": {
	    "a": {"dependencies": ["a"]},
	    "b": {"extends": "a", "dependencies": ["b"]},
	    "c": {"extends": "a", "dependencies": ["c"]},
	    "d": {"extends": ["b", "c"], "dependencies": ["d"]}
	  }
	}`)
	chain, err := ResolveChain(document, nil, "d")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if !reflect.DeepEqual(chain, []string{"a", "b", "c", "d"}) {
		t.Errorf("chain = %v, want each profile exactly once with parents first", chain)
	}
}

func TestExtendsCycleIsFatalAndNamesThePath(t *testing.T) {
	document := doc(t, `{"profiles":{"a":{"extends":"b"},"b":{"extends":"a"}}}`)
	_, err := ResolveChain(document, nil, "a")
	if err == nil {
		t.Fatal("an inheritance cycle was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "->") {
		t.Errorf("error should name the cycle path: %v", err)
	}
}

func TestExtendsSelfIsACycle(t *testing.T) {
	if _, err := ResolveChain(doc(t, `{"profiles":{"a":{"extends":"a"}}}`), nil, "a"); err == nil {
		t.Error("a profile extending itself was accepted")
	}
}

func TestExtendsUnknownParentIsFatal(t *testing.T) {
	_, err := ResolveChain(doc(t, `{"profiles":{"a":{"extends":"missing"}}}`), nil, "a")
	if err == nil {
		t.Fatal("an unknown parent was accepted")
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "'a'") {
		t.Errorf("error should name both the child and the missing parent: %v", err)
	}
}

func TestExtendsIsMergedAcrossFiles(t *testing.T) {
	// The same profile name declares different parents in each file; the effective list is
	// the array merge of both, so a project profile can extend a globally-defined one.
	global := doc(t, `{"profiles":{"base-a":{"dependencies":["a"]},"leaf":{"extends":"base-a"}}}`)
	project := doc(t, `{"profiles":{"base-b":{"dependencies":["b"]},"leaf":{"extends":"base-b"}}}`)

	chain, err := ResolveChain(global, project, "leaf")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if !reflect.DeepEqual(chain, []string{"base-a", "base-b", "leaf"}) {
		t.Errorf("chain = %v", chain)
	}
}

func TestExtendsParentDefinedOnlyInTheOtherFile(t *testing.T) {
	global := doc(t, `{"profiles":{"parent":{"dependencies":["from-global"]}}}`)
	project := doc(t, `{"profiles":{"child":{"extends":"parent","dependencies":["from-project"]}}}`)
	settings := merged(t, mustJSON(t, global), mustJSON(t, project), "child")
	if !reflect.DeepEqual(settings.Dependencies, []string{"from-global", "from-project"}) {
		t.Errorf("dependencies = %v", settings.Dependencies)
	}
}

func TestProfileIsAdditiveOnly(t *testing.T) {
	// A profile cannot narrow the base: both allow entries survive. This is the documented
	// trade-off that makes effective permissions readable, and the reason for the
	// minimal-base pattern.
	settings := merged(t,
		`{"network":{"allow":["broad.example.com"]},"profiles":{"narrow":{"network":{"allow":["narrow.example.com"]}}}}`,
		`{}`, "narrow")
	if len(settings.Network.Allow) != 2 {
		t.Errorf("allow = %v, want both entries (profiles only add)", settings.Network.Allow)
	}
}

func TestAgentArgsConcatenateWithoutDeduplication(t *testing.T) {
	// Generic array dedup would corrupt an argument vector: the second `--allowedTools` would
	// vanish and its value would bind to the first flag.
	settings := merged(t,
		`{"agents":{"claude":{"args":["--allowedTools","a"]}},"profiles":{"p":{"agents":{"claude":{"args":["--allowedTools","b"]}}}}}`,
		`{}`, "p")
	want := []string{"--allowedTools", "a", "--allowedTools", "b"}
	if !reflect.DeepEqual(settings.AgentArgs("claude"), want) {
		t.Errorf("args = %v, want %v", settings.AgentArgs("claude"), want)
	}
}

func TestAgentArgsConcatenateAcrossAllFourSources(t *testing.T) {
	settings := merged(t,
		`{"agents":{"claude":{"args":["gb"]}},"profiles":{"p":{"agents":{"claude":{"args":["go"]}}}}}`,
		`{"agents":{"claude":{"args":["pb"]}},"profiles":{"p":{"agents":{"claude":{"args":["po"]}}}}}`,
		"p")
	want := []string{"gb", "go", "pb", "po"}
	if !reflect.DeepEqual(settings.AgentArgs("claude"), want) {
		t.Errorf("args = %v, want %v", settings.AgentArgs("claude"), want)
	}
}

func TestAgentArgsWithoutProfileStillConcatenate(t *testing.T) {
	settings := merged(t,
		`{"agents":{"claude":{"args":["--model","sonnet"]}}}`,
		`{"agents":{"claude":{"args":["--model","opus"]}}}`,
		"")
	// The args exception is not a profile feature: the no-profile path runs through the same
	// merge, so a repeated flag survives there too.
	want := []string{"--model", "sonnet", "--model", "opus"}
	if got := settings.AgentArgs("claude"); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestAgentArgsOfAnotherAgentAreIgnored(t *testing.T) {
	settings := merged(t, `{"agents":{"gemini":{"args":["--yolo"]}}}`, `{}`, "")
	if got := settings.AgentArgs("claude"); got != nil {
		t.Errorf("claude args = %v, want none", got)
	}
	if got := settings.AgentArgs("gemini"); !reflect.DeepEqual(got, []string{"--yolo"}) {
		t.Errorf("gemini args = %v", got)
	}
}

func TestProfileOverlayCarriesEveryRootSetting(t *testing.T) {
	settings := merged(t, `{}`, `{"profiles":{"p":{
	  "files": {"exclude": [".env"], "include": {"~/.npmrc": "~/.npmrc"}},
	  "network": {"allow": ["api.github.com"], "hostGatewayDomains": ["mydb.local:5432"], "subnetPool": "10.99.0.0/16"},
	  "dependencies": ["make"],
	  "container": {"docker": true, "memoryLimit": "8g", "baseImage": "ubuntu:24.04", "enabledAgents": ["claude"]},
	  "hooks": {"setup": [{"script": "s.sh"}], "prestart": [{"script": "p.sh"}]},
	  "libraries": {"/lib": "/libs/lib"},
	  "environment": {"MODE": "profiled"},
	  "agents": {"claude": {"args": ["--model", "opus"]}}
	}}}`, "p")

	if len(settings.Files.Exclude) != 1 || settings.Files.Include["~/.npmrc"] == "" {
		t.Error("files settings did not come through the overlay")
	}
	if len(settings.Network.Allow) != 1 || settings.Network.SubnetPool != "10.99.0.0/16" {
		t.Error("network settings did not come through the overlay")
	}
	if !settings.Container.Docker || settings.Container.MemoryLimit != "8g" {
		t.Error("container settings did not come through the overlay")
	}
	if len(settings.Hooks.Setup) != 1 || len(settings.Hooks.Prestart) != 1 {
		t.Error("hooks did not come through the overlay")
	}
	if settings.Environment["MODE"] != "profiled" || len(settings.Libraries) != 1 {
		t.Error("environment or libraries did not come through the overlay")
	}
	if !reflect.DeepEqual(settings.AgentArgs("claude"), []string{"--model", "opus"}) {
		t.Error("agent args did not come through the overlay")
	}
}

func TestProfileNamesLists(t *testing.T) {
	names := ProfileNames(doc(t, `{"profiles":{"z":{},"a":{},"m":{}}}`))
	if !reflect.DeepEqual(names, []string{"a", "m", "z"}) {
		t.Errorf("ProfileNames = %v", names)
	}
	if got := ProfileNames(nil); len(got) != 0 {
		t.Errorf("ProfileNames(nil) = %v", got)
	}
}

func mustJSON(t *testing.T, document Document) string {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
