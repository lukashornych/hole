package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAcceptsKnownSettings(t *testing.T) {
	valid := []string{
		`{}`,
		`{"$schema":"https://example.com/schema.json"}`,
		`{"files":{"exclude":[".env"],"include":{"~/.npmrc":"~/.npmrc"}}}`,
		`{"network":{"allow":["api.github.com","*.npmjs.org","db.example.com:5432","github.com:22,443","10.0.0.5:22","192.168.1.0/24:8080"],"hostGatewayDomains":["mydb.local:5432","myapi.local:5432,8080"]}}`,
		`{"network":{"subnetPool":"10.222.0.0/16"}}`,
		`{"network":{"bridgeNetfilterFix":"auto"}}`,
		`{"network":{"bridgeNetfilterFix":"off"}}`,
		`{"dependencies":["make","gcc=4:13.2.0-7ubuntu1"]}`,
		`{"container":{"memoryLimit":"8g","memorySwapLimit":"8g","docker":true,"baseImage":"ubuntu:24.04","enabledAgents":["claude"]}}`,
		// Agent names are a pattern, not a closed enum: user agents in ~/.hole/agents are
		// valid here and are checked against the registry at startup.
		`{"container":{"enabledAgents":["my-agent"]}}`,
		`{"hooks":{"setup":[{"script":".hole/setup.sh"},{"script":".hole/setup.d/*.sh"}],"prestart":[{"script":"a.sh"}],"setupHost":[{"script":"b.sh"}],"cleanupHost":[{"script":"c.sh"}]}}`,
		`{"libraries":{"/host/lib":"/container/lib","~/other":{"path":"~/other","readwrite":true}}}`,
		`{"environment":{"KEY":"value"}}`,
		`{"agents":{"claude":{"args":["--model","opus"]}}}`,
		`{"git":{"worktreeLinks":"rw","worktreePool":true}}`,
		// The pool is a per-profile decision too, which is why it lives in $defs/settings.
		`{"profiles":{"p":{"git":{"worktreePool":true}}}}`,
		// A profile accepts exactly the root settings keys, plus extends in either form.
		`{"profiles":{"research":{"network":{"allow":["*.wikipedia.org"]}}}}`,
		`{"profiles":{"a":{},"b":{"extends":"a"},"c":{"extends":["a","b"]}}}`,
		`{"profiles":{"p":{"agents":{"claude":{"args":["--model","opus"]}},"dependencies":["make"]}}}`,
	}
	for _, document := range valid {
		if err := ValidateBytes([]byte(document), "test"); err != nil {
			t.Errorf("valid document rejected: %s\n%v", document, err)
		}
	}
}

func TestValidateRejectsUnknownAndMalformedSettings(t *testing.T) {
	invalid := map[string]string{
		"unknown top-level key":    `{"nope":true}`,
		"unknown nested key":       `{"container":{"nope":true}}`,
		"wrong type":               `{"dependencies":"make"}`,
		"malformed allow entry":    `{"network":{"allow":["not a host"]}}`,
		"allow entry with spaces":  `{"network":{"allow":["example.com :443"]}}`,
		"removed allowedPorts":     `{"network":{"allowedPorts":[443]}}`,
		"removed domainWhitelist":  `{"network":{"domainWhitelist":["example.com"]}}`,
		"scalar hooks.setup":       `{"hooks":{"setup":{"script":"a.sh"}}}`,
		"malformed agent name":     `{"container":{"enabledAgents":["Not An Agent"]}}`,
		"library relative path":    `{"libraries":{"lib":"relative/path"}}`,
		"host gateway bad port":    `{"network":{"hostGatewayDomains":["mydb.local:notaport"]}}`,
		"host gateway no port":     `{"network":{"hostGatewayDomains":["mydb.local"]}}`,
		"malformed subnet pool":    `{"network":{"subnetPool":"not-a-cidr"}}`,
		"bad bridge netfilter fix": `{"network":{"bridgeNetfilterFix":"maybe"}}`,
		"hook entry without path":  `{"hooks":{"prestart":[{}]}}`,
		// Profiles cannot nest, cannot redeclare $schema, and are strict about their keys.
		"nested profiles":          `{"profiles":{"p":{"profiles":{"q":{}}}}}`,
		"schema inside a profile":  `{"profiles":{"p":{"$schema":"x"}}}`,
		"unknown key in profile":   `{"profiles":{"p":{"nope":true}}}`,
		"bad profile name":         `{"profiles":{"Bad Name":{}}}`,
		"bad extends name":         `{"profiles":{"p":{"extends":"Bad Name"}}}`,
		"extends wrong type":       `{"profiles":{"p":{"extends":42}}}`,
		"unknown agent arg key":    `{"agents":{"claude":{"nope":true}}}`,
		"agent args wrong type":    `{"agents":{"claude":{"args":"--model"}}}`,
		"bad agent name":           `{"agents":{"Not An Agent":{"args":[]}}}`,
		"worktree pool not a bool": `{"git":{"worktreePool":"yes"}}`,
		"unknown git key":          `{"git":{"worktreesDir":"~/wt"}}`,
	}
	for name, document := range invalid {
		t.Run(name, func(t *testing.T) {
			err := ValidateBytes([]byte(document), "test")
			if err == nil {
				t.Fatalf("invalid document accepted: %s", document)
			}
			var failure *ValidationFailure
			if !errors.As(err, &failure) {
				t.Fatalf("expected a ValidationFailure, got %T", err)
			}
			if len(failure.Details) == 0 {
				t.Error("validation failure carries no detail lines")
			}
		})
	}
}

func TestValidateMissingFileIsNotAnError(t *testing.T) {
	if err := Validate(filepath.Join(t.TempDir(), "absent.json"), "test"); err != nil {
		t.Errorf("missing settings file must be accepted: %v", err)
	}
}

func TestValidateReportsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Validate(path, "project settings"); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

func TestLoadMissingFileYieldsNilDocument(t *testing.T) {
	document, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if document != nil {
		t.Errorf("expected nil document, got %v", document)
	}
}
