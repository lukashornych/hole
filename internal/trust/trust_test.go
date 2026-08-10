package trust

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
)

// document parses a settings document from JSON, as config.Load would.
func document(t *testing.T, raw string) config.Document {
	t.Helper()
	var doc config.Document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return doc
}

func TestGrants(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     []grant
	}{
		{
			name:     "no project file",
			document: `null`,
		},
		{
			name: "settings confined to the sandbox are not gated",
			document: `{
				"files": {"exclude": [".env"]},
				"network": {"subnetPool": "10.99.0.0/16"},
				"environment": {"CI": "1"},
				"agents": {"claude": {"args": ["--model", "opus"]}},
				"container": {"baseImage": "ubuntu:24.04", "memoryLimit": "8g"},
				"hooks": {"prestart": [{"script": ".hole/prestart.sh"}]}
			}`,
		},
		{
			name: "host-affecting settings are reported in capability order",
			document: `{
				"dependencies": ["maven"],
				"container": {"docker": true},
				"libraries": {"~/lib": {"path": "/libs/lib", "readwrite": true}},
				"files": {"include": {"~/.ssh": "~/.ssh"}},
				"network": {"allow": ["registry.npmjs.org"], "hostGatewayDomains": ["db.local:5432"]},
				"hooks": {
					"setup": [{"script": ".hole/setup.sh"}],
					"setupHost": [{"script": ".hole/setup-host.sh"}],
					"cleanupHost": [{"script": ".hole/cleanup-host.sh"}]
				}
			}`,
			want: []grant{
				{Key: "hooks.setupHost", Values: []string{".hole/setup-host.sh"}},
				{Key: "hooks.cleanupHost", Values: []string{".hole/cleanup-host.sh"}},
				{Key: "files.include", Values: []string{"~/.ssh -> ~/.ssh"}},
				{Key: "libraries", Values: []string{"~/lib -> /libs/lib (read-write)"}},
				{Key: "container.docker", Values: []string{"true"}},
				{Key: "network.hostGatewayDomains", Values: []string{"db.local:5432"}},
				{Key: "hooks.setup", Values: []string{".hole/setup.sh"}},
				{Key: "dependencies", Values: []string{"maven"}},
				{Key: "network.allow", Values: []string{"registry.npmjs.org"}},
			},
		},
		{
			// Egress leaves the sandbox, so widening it is the repository's request to make
			// visible — including from a profile, which is part of the same file.
			name: "network.allow is gated in the base settings and in profiles",
			document: `{
				"network": {"allow": ["registry.npmjs.org"]},
				"profiles": {"research": {"network": {"allow": ["*.wikipedia.org"]}}}
			}`,
			want: []grant{
				{Key: "network.allow", Values: []string{"registry.npmjs.org", "*.wikipedia.org"}},
			},
		},
		{
			name:     "docker disabled explicitly asks for nothing",
			document: `{"container": {"docker": false}}`,
		},
		{
			// A profile is part of the same file, so its grants are shown whether or not the
			// run selects it — the file is what gets trusted.
			name: "profiles are scanned alongside the base settings",
			document: `{
				"files": {"include": {"~/.npmrc": "~/.npmrc"}},
				"profiles": {
					"review": {"files": {"include": {"~/.npmrc": "~/.npmrc"}}},
					"deploy": {"hooks": {"setupHost": [{"script": ".hole/deploy.sh"}]}}
				}
			}`,
			want: []grant{
				{Key: "hooks.setupHost", Values: []string{".hole/deploy.sh"}},
				{Key: "files.include", Values: []string{"~/.npmrc -> ~/.npmrc"}},
			},
		},
		{
			// A profile that only inherits asks for nothing of its own: the parent it names may
			// live in the *global* file, which is the user's own document and never gated.
			name:     "a profile that only extends another asks for nothing",
			document: `{"profiles": {"review": {"extends": "base"}}}`,
		},
		{
			// Raw values are what the user sees and what the digest covers: an expanded path
			// embeds the host's home, and a redirected $VAR would leave the digest unchanged.
			name:     "values stay exactly as written",
			document: `{"files": {"include": {"$SECRETS/id_rsa": "~/.ssh/id_rsa"}}}`,
			want: []grant{
				{Key: "files.include", Values: []string{"$SECRETS/id_rsa -> ~/.ssh/id_rsa"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grants, err := requestedGrants(document(t, test.document))
			if err != nil {
				t.Fatal(err)
			}
			if len(grants) != len(test.want) {
				t.Fatalf("grants = %v, want %v", grantKeys(grants), grantKeys(test.want))
			}
			for i, want := range test.want {
				if grants[i].Key != want.Key || !reflect.DeepEqual(grants[i].Values, want.Values) {
					t.Errorf("grant %d = %s %v, want %s %v", i,
						grants[i].Key, grants[i].Values, want.Key, want.Values)
				}
				if grants[i].Effect == "" {
					t.Errorf("grant %s has no effect description", grants[i].Key)
				}
			}
		})
	}
}

func TestDigest(t *testing.T) {
	base := `{"hooks": {"setupHost": [{"script": "a.sh"}]}}`

	digestOf := func(raw string) string {
		grants, err := requestedGrants(document(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		return digestOf(grants)
	}

	// An edit that touches nothing gated keeps the decision valid, so the user is not asked
	// again for a settings change that grants nothing.
	if digestOf(base) != digestOf(`{
		"hooks": {"setupHost": [{"script": "a.sh"}]},
		"environment": {"CI": "1"},
		"files": {"exclude": [".env"]}
	}`) {
		t.Error("an ungated addition changed the digest")
	}
	if digestOf(base) == digestOf(`{"hooks": {"setupHost": [{"script": "a.sh"}, {"script": "b.sh"}]}}`) {
		t.Error("a widened grant kept the same digest")
	}
	// Egress is gated, so a project that starts reaching one more destination asks again —
	// including for the user's own projects.
	if digestOf(`{"network": {"allow": ["a.example.com"]}}`) ==
		digestOf(`{"network": {"allow": ["a.example.com", "b.example.com"]}}`) {
		t.Error("adding a domain to network.allow kept the same digest")
	}
	if digestOf(base) == digestOf(`{"hooks": {"cleanupHost": [{"script": "a.sh"}]}}`) {
		t.Error("the same value under a different key kept the same digest")
	}
}

// gateOptions builds a Gate call against a fresh trust store.
func gateOptions(t *testing.T, raw, answer string) (Options, *bytes.Buffer, *Store) {
	t.Helper()
	store := NewStore(t.TempDir())
	out := &bytes.Buffer{}
	return Options{
		ProjectDir:   "/projects/repo",
		SettingsFile: filepath.Join("/projects/repo", ".hole", "settings.json"),
		Document:     document(t, raw),
		Store:        store,
		Interactive:  true,
		In:           strings.NewReader(answer),
		Out:          out,
	}, out, store
}

func TestGateDoesNotPromptForUngatedSettings(t *testing.T) {
	opts, out, store := gateOptions(t, `{"files": {"exclude": [".env"]}, "environment": {"CI": "1"}}`, "")
	if err := Gate(opts); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("prompted for ungated settings:\n%s", out)
	}
	if trustFileExists(store) {
		t.Error("recorded a decision for a project that asked for nothing")
	}
}

// A settings file that only widens egress used to be the canonical no-prompt document. It is
// gated now: the gateway policing a policy the repository wrote is not a boundary.
func TestGateAsksForNetworkOnlyProjects(t *testing.T) {
	opts, out, _ := gateOptions(t, `{"network": {"allow": ["attacker.example.com"]}}`, "y\n")
	if err := Gate(opts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"network.allow", "attacker.example.com"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompt does not mention %q:\n%s", want, out)
		}
	}

	headless, _, _ := gateOptions(t, `{"network": {"allow": ["attacker.example.com"]}}`, "")
	headless.Interactive = false
	err := Gate(headless)
	if err == nil {
		t.Fatal("a non-interactive run accepted an untrusted network-only project")
	}
	for _, want := range []string{"network.allow", "--trust-project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestGateRecordsAnAcceptedProject(t *testing.T) {
	settings := `{"hooks": {"setupHost": [{"script": ".hole/setup-host.sh"}]}}`
	opts, out, store := gateOptions(t, settings, "y\n")
	if err := Gate(opts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hooks.setupHost", ".hole/setup-host.sh", opts.SettingsFile} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompt does not mention %q:\n%s", want, out)
		}
	}

	// A second start with the same grants must not ask again.
	next, nextOut, _ := gateOptions(t, settings, "")
	next.Store = store
	if err := Gate(next); err != nil {
		t.Fatal(err)
	}
	if nextOut.Len() != 0 {
		t.Errorf("prompted again for an already trusted project:\n%s", nextOut)
	}

	// Widening what the project asks for asks again, and refusing aborts the start.
	wider, _, _ := gateOptions(t, `{"hooks": {"setupHost": [{"script": ".hole/setup-host.sh"}]},
		"files": {"include": {"~/.ssh": "~/.ssh"}}}`, "n\n")
	wider.Store = store
	if err := Gate(wider); err == nil {
		t.Error("a widened grant set was accepted without asking")
	}
}

func TestGateRefusalAborts(t *testing.T) {
	tests := map[string]string{
		"no":            "n\n",
		"empty answer":  "\n",
		"closed stdin":  "",
		"anything else": "maybe\n",
	}
	for name, answer := range tests {
		t.Run(name, func(t *testing.T) {
			opts, _, store := gateOptions(t, `{"container": {"docker": true}}`, answer)
			err := Gate(opts)
			if err == nil {
				t.Fatal("Gate accepted a refused project")
			}
			if !strings.Contains(err.Error(), opts.SettingsFile) {
				t.Errorf("error does not name the settings file: %v", err)
			}
			if trustFileExists(store) {
				t.Error("recorded a decision the user declined")
			}
		})
	}
}

// Without a terminal there is nobody to ask, so the start fails rather than granting silently —
// and the message has to name the file and the keys, since a CI log is all the user will see.
func TestGateWithoutATerminal(t *testing.T) {
	opts, _, _ := gateOptions(t, `{"libraries": {"~/lib": "/libs/lib"}}`, "")
	opts.Interactive = false
	err := Gate(opts)
	if err == nil {
		t.Fatal("Gate accepted an untrusted project with no terminal")
	}
	for _, want := range []string{opts.SettingsFile, "libraries", "--trust-project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestGatePreApproved(t *testing.T) {
	opts, out, store := gateOptions(t, `{"libraries": {"~/lib": "/libs/lib"}}`, "")
	opts.Interactive = false
	opts.PreApproved = true
	if err := Gate(opts); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("--trust-project still prompted:\n%s", out)
	}
	if !store.Trusted(opts.ProjectDir, digestOf(mustGrants(t, opts.Document))) {
		t.Error("--trust-project did not record the decision")
	}
}

// trustFileExists reports whether any decision has been written at all.
func trustFileExists(store *Store) bool {
	_, err := os.Stat(store.Path())
	return err == nil
}

func mustGrants(t *testing.T, doc config.Document) []grant {
	t.Helper()
	grants, err := requestedGrants(doc)
	if err != nil {
		t.Fatal(err)
	}
	return grants
}
