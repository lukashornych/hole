package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/compose"
	"github.com/lukashornych/hole/v2/internal/config"
	"gopkg.in/yaml.v3"
)

func rootlessCompose(t *testing.T, in composeInput) (*compose.File, []byte) {
	t.Helper()
	path, _, err := generateCompose(in)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// ServiceVolume uses custom marshaling; inspect services using a generic map below.
	var file compose.File
	// Skip volumes when decoding the typed service model.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, svc := range raw["services"].(map[string]any) {
		delete(svc.(map[string]any), "volumes")
	}
	stripped, _ := yaml.Marshal(raw)
	if err := yaml.Unmarshal(stripped, &file); err != nil {
		t.Fatal(err)
	}
	return &file, data
}

func TestRootlessComposeIsolationAndRouting(t *testing.T) {
	project, lib := fixture(t)
	settings := &config.Settings{}
	settings.Container.Docker = true
	settings.Container.DockerRootless = true
	settings.Environment = map[string]string{"PATH": "/hostile", "DOCKERD": "/hostile/dockerd", "DOCKER_HOST": "tcp://other:2375"}
	settings.Libraries = map[string]config.Library{lib: {Path: "/libs/shared", ReadWrite: true}}
	in := testInput(t, project, t.TempDir(), settings, Options{})
	in.host.UID = "123456"
	in.host.GID = "234567"
	f, data := rootlessCompose(t, in)
	sidecar := f.Services["docker"]
	if sidecar.Privileged || sidecar.User != "123456:234567" {
		t.Fatalf("privilege/identity: %+v", sidecar)
	}
	if !reflect.DeepEqual(sidecar.CapAdd, []string{"SETUID", "SETGID"}) || !reflect.DeepEqual(sidecar.CapDrop, []string{"ALL"}) {
		t.Fatalf("unexpected capabilities: %+v", sidecar)
	}
	if !reflect.DeepEqual(sidecar.Devices, []string{"/dev/net/tun:/dev/net/tun:rw"}) {
		t.Fatalf("TUN must be the only passed device: %v", sidecar.Devices)
	}
	if sidecar.Build.Args["SUBID_START"] != "234568" {
		t.Fatal("subordinate IDs overlap mapped user")
	}
	for _, v := range sidecar.Environment {
		if strings.Contains(v, "hostile") || strings.Contains(v, "other") {
			t.Fatalf("inherited daemon environment: %s", v)
		}
	}
	if len(sidecar.Networks) != 1 || sidecar.Networks["sandbox"] == nil {
		t.Fatal("sidecar bypasses gateway")
	}
	route := f.Services["docker-route"]
	if route.Privileged || route.NetworkMode != "service:docker" || !reflect.DeepEqual(route.CapAdd, []string{"NET_ADMIN"}) {
		t.Fatalf("route helper: %+v", route)
	}
	if len(route.Devices) != 0 {
		t.Fatal("route helper must not receive TUN")
	}
	if route.DependsOn["docker"].Condition != "service_started" {
		t.Fatal("circular health dependency")
	}
	agent := f.Services["agent"]
	if agent.DependsOn["docker-route"].Condition != "service_completed_successfully" || agent.DependsOn["docker"].Condition != "service_healthy" {
		t.Fatal("hooks can run before routing/daemon ready")
	}
	if !strings.Contains(string(data), lib+":/libs/shared") {
		t.Fatal("RW library mirror missing")
	}
	raw, err := os.ReadFile(filepath.Join(in.runTmpDir, "rootless-daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["rootless"] != true || cfg["data-root"] != dindDataRoot || cfg["exec-root"] != "/run/user/123456/exec" {
		t.Fatalf("config: %s", raw)
	}
	if !reflect.DeepEqual(cfg["dns"], []any{in.gatewayIP}) {
		t.Fatalf("DNS can bypass policy: %s", raw)
	}
	if _, exists := cfg["bridge"]; exists {
		t.Fatal("plain docker run must retain its default network")
	}
}

func TestRootlessOffByteIdentical(t *testing.T) {
	project, _ := fixture(t)
	settings := &config.Settings{}
	settings.Container.Docker = true
	in := testInput(t, project, t.TempDir(), settings, Options{})
	_, before := rootlessCompose(t, in)
	settings.Container.DockerRootless = true
	rootlessCompose(t, in)
	settings.Container.DockerRootless = false
	_, after := rootlessCompose(t, in)
	if string(before) != string(after) {
		t.Fatal("rootless opt-out changed compose output")
	}
	// Existing TestGenerateComposeGolden pins this result against pre-feature files.
}

func TestRootlessSettingsPrerequisites(t *testing.T) {
	settings := &config.Settings{}
	settings.Container.DockerRootless = true
	host := testHost()
	if err := validateRootlessSettings(settings, false, host); err == nil || !strings.Contains(err.Error(), "container.docker") {
		t.Fatal(err)
	}
	host.UID = "0"
	if err := validateRootlessSettings(settings, true, host); err == nil || !strings.Contains(err.Error(), "non-root") {
		t.Fatal(err)
	}
	host.UID = "invalid"
	if err := validateRootlessSettings(settings, true, host); err == nil {
		t.Fatal("invalid UID accepted")
	}
	settings.Container.DockerRootless = false
	if err := validateRootlessSettings(settings, false, host); err != nil {
		t.Fatal("legacy path changed", err)
	}
}

func TestRootlessMaterializesAllImageSources(t *testing.T) {
	project, _ := fixture(t)
	settings := &config.Settings{}
	settings.Container.Docker = true
	settings.Container.DockerRootless = true
	in := testInput(t, project, t.TempDir(), settings, Options{})
	file, _ := rootlessCompose(t, in)
	for _, name := range []string{"Dockerfile", "launch.sh", "daemon.sh", "health.sh", "hole-dockerd-netns"} {
		if _, err := os.Stat(filepath.Join(file.Services["docker"].Build.Context, name)); err != nil {
			t.Fatal(err)
		}
	}
}
