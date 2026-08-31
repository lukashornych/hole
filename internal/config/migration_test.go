package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRemovedKeysAcceptsCurrentSettings(t *testing.T) {
	current := []string{
		`{}`,
		`{"network":{"allow":["api.github.com"]}}`,
		`{"hooks":{"setup":[{"script":"a.sh"}]}}`,
		`{"hooks":{"prestart":[{"script":"a.sh"}]}}`,
	}
	for _, document := range current {
		if err := CheckRemovedKeys("test", doc(t, document)); err != nil {
			t.Errorf("current settings rejected: %s\n%v", document, err)
		}
	}
}

func TestCheckRemovedKeysReportsDomainWhitelist(t *testing.T) {
	err := CheckRemovedKeys("project settings", doc(t,
		`{"network":{"domainWhitelist":["example.com","10.0.0.5"],"allowedPorts":[443,8080]}}`))
	if err == nil {
		t.Fatal("removed keys were accepted")
	}
	var migration *MigrationError
	if !errors.As(err, &migration) {
		t.Fatalf("expected a MigrationError, got %T", err)
	}

	message := err.Error()
	// The hint has to be paste-ready: both keys named, the replacement key named, and the
	// translated entries spelled out with their ports.
	for _, want := range []string{
		"network.domainWhitelist",
		"network.allowedPorts",
		"network.allow",
		`"example.com:443,8080"`,
		`"*.example.com:443,8080"`,
		`"10.0.0.5:443,8080"`,
		MigrationURL,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("migration error is missing %q:\n%s", want, message)
		}
	}
	// An address has no subdomains, so it must not get a wildcard twin.
	if strings.Contains(message, `"*.10.0.0.5`) {
		t.Errorf("an IP entry was given a wildcard twin:\n%s", message)
	}
}

func TestCheckRemovedKeysUsesDefaultPortsWhenUnset(t *testing.T) {
	err := CheckRemovedKeys("test", doc(t, `{"network":{"domainWhitelist":["example.com"]}}`))
	if err == nil {
		t.Fatal("removed key was accepted")
	}
	// Without allowedPorts the old behavior was ports 80 and 443, i.e. the new default, so
	// the hint suggests entries without a port suffix.
	if !strings.Contains(err.Error(), `"example.com"`) {
		t.Errorf("hint should suggest the default-port form:\n%s", err)
	}
}

func TestCheckRemovedKeysExplainsEmptyAllowedPorts(t *testing.T) {
	err := CheckRemovedKeys("test", doc(t, `{"network":{"domainWhitelist":["example.com"],"allowedPorts":[]}}`))
	if err == nil {
		t.Fatal("removed key was accepted")
	}
	// `allowedPorts: []` used to emit `ConnectPort 0` and block everything; the hint must say
	// so rather than silently suggesting reachable entries.
	if !strings.Contains(err.Error(), "used to block all traffic") {
		t.Errorf("hint does not explain the empty-ports case:\n%s", err)
	}
}

func TestCheckRemovedKeysReportsScalarSetupHook(t *testing.T) {
	err := CheckRemovedKeys("test", doc(t, `{"hooks":{"setup":{"script":".hole/setup.sh"}}}`))
	if err == nil {
		t.Fatal("the scalar hooks.setup form was accepted")
	}
	if !strings.Contains(err.Error(), `"setup": [{ "script": ".hole/setup.sh" }]`) {
		t.Errorf("hint does not show the array form:\n%s", err)
	}
}

func TestCheckRemovedKeysReportsBothAtOnce(t *testing.T) {
	err := CheckRemovedKeys("test", doc(t,
		`{"network":{"domainWhitelist":["a.com"]},"hooks":{"setup":{"script":"a.sh"}}}`))
	if err == nil {
		t.Fatal("removed keys were accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "network.allow") || !strings.Contains(message, "hooks.setup is now an array") {
		t.Errorf("both removals should be reported in one error:\n%s", message)
	}
}

func TestLoadAndValidatePrefersTheMigrationErrorOverTheSchemaError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"network":{"domainWhitelist":["example.com"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAndValidate(path, "project settings")
	if err == nil {
		t.Fatal("removed key was accepted")
	}
	var migration *MigrationError
	if !errors.As(err, &migration) {
		t.Fatalf("expected a MigrationError rather than a bare schema failure, got %T: %v", err, err)
	}
}

func TestLoadAndValidateAcceptsCurrentFileAndSkipsMissingOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"network":{"allow":["api.github.com:443"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := LoadAndValidate(path, "test")
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if document == nil {
		t.Fatal("expected a document")
	}

	missing, err := LoadAndValidate(filepath.Join(dir, "absent.json"), "test")
	if err != nil {
		t.Errorf("a missing settings file must be accepted: %v", err)
	}
	if missing != nil {
		t.Errorf("a missing settings file must yield a nil document, got %v", missing)
	}
}
