//go:build integration

// Integration tests for the version-change cleanup. They fabricate the resources a 1.x
// installation would have left behind and check that the migration removes exactly those:
// run them with `make itest`.
package update

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hostenv"
)

func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	containerEngine, err := engine.Detect()
	if err != nil {
		t.Skipf("no container runtime available: %v", err)
	}
	return containerEngine
}

func TestRemoveLegacyResourcesRemovesOnlyLegacyOnes(t *testing.T) {
	containerEngine := testEngine(t)

	// A 1.x-era volume, and the shared DinD cache the pull-through registry replaced.
	legacyVolumes := []string{legacyDockerCache, "hole-sandbox-agent-home-itest"}
	for _, volume := range legacyVolumes {
		if err := containerEngine.VolumeCreate(volume, nil); err != nil {
			t.Fatalf("create %s: %v", volume, err)
		}
		t.Cleanup(func() { _ = containerEngine.VolumeRemove(volume) })
	}

	// A 1.x network: Hole-named but unlabeled, which is exactly what distinguishes it from a
	// network this version created and has not attached anything to yet.
	legacyNetwork := "hole-sandbox-itest-legacy-abc123_sandbox"
	_ = containerEngine.NetworkRemove(legacyNetwork)
	if err := containerEngine.NetworkCreate(engine.NetworkOptions{
		Name:   legacyNetwork,
		Subnet: netip.MustParsePrefix("10.223.250.0/24"),
	}); err != nil {
		t.Fatalf("create %s: %v", legacyNetwork, err)
	}
	t.Cleanup(func() { _ = containerEngine.NetworkRemove(legacyNetwork) })

	// A current network: labeled, so it must survive even though it is unattached.
	currentNetwork := "hole-sandbox-itest-current-abc123_sandbox"
	_ = containerEngine.NetworkRemove(currentNetwork)
	if err := containerEngine.NetworkCreate(engine.NetworkOptions{
		Name:   currentNetwork,
		Subnet: netip.MustParsePrefix("10.223.251.0/24"),
		Labels: map[string]string{engine.LabelManaged: "true"},
	}); err != nil {
		t.Fatalf("create %s: %v", currentNetwork, err)
	}
	t.Cleanup(func() { _ = containerEngine.NetworkRemove(currentNetwork) })

	// And something that is not Hole's at all.
	foreignVolume := "itest-not-hole"
	if err := containerEngine.VolumeCreate(foreignVolume, nil); err != nil {
		t.Fatalf("create %s: %v", foreignVolume, err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(foreignVolume) })

	RemoveLegacyResources(containerEngine)

	for _, volume := range legacyVolumes {
		if containerEngine.VolumeExists(volume) {
			t.Errorf("legacy volume %s survived the migration", volume)
		}
	}
	if containerEngine.NetworkExists(legacyNetwork) {
		t.Errorf("legacy network %s survived the migration", legacyNetwork)
	}
	if !containerEngine.NetworkExists(currentNetwork) {
		t.Error("the migration removed a labeled network belonging to this version")
	}
	if !containerEngine.VolumeExists(foreignVolume) {
		t.Error("the migration removed a volume that is not Hole's")
	}
}

func TestRemoveLegacyResourcesOnACleanMachine(t *testing.T) {
	// Must be a silent no-op rather than an error.
	RemoveLegacyResources(testEngine(t))
}

func TestUninstallKeepsUserDataUnlessAsked(t *testing.T) {
	containerEngine := testEngine(t)
	host := testHost(t)

	// Populate the user directory the way a real installation would.
	if err := SaveState(host, State{LastVersion: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(host.UserAgentsDir(), "mine")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// KeepBinary, or this would delete the test binary itself.
	Uninstall(host, containerEngine, UninstallOptions{RemoveSettings: false, KeepBinary: true})

	if _, err := os.Stat(agentDir); err != nil {
		t.Error("uninstall removed a custom agent without being asked to remove user data")
	}

	Uninstall(host, containerEngine, UninstallOptions{RemoveSettings: true, KeepBinary: true})

	if _, err := os.Stat(host.HoleDir()); err == nil {
		t.Error("uninstall did not remove the user directory when asked to")
	}
}

func testHost(t *testing.T) hostenv.Host {
	t.Helper()
	return hostenv.Host{Home: t.TempDir(), Username: "tester"}
}
