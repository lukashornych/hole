//go:build integration

// Integration tests for the pull-through image cache. They need a real daemon and pull the
// upstream registry image: run them with `make itest`.
package dindregistry

import (
	"net/netip"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
)

func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	containerEngine, err := engine.Detect()
	if err != nil {
		t.Skipf("no container runtime available: %v", err)
	}
	return containerEngine
}

// waitUntilGone blocks until the mirror container is really gone. `rm -f` returns before the
// daemon finishes, and a container mid-removal is neither running nor startable — so a test that
// removes and immediately calls Ensure can find one it cannot use.
func waitUntilGone(t *testing.T, containerEngine *engine.Engine) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if len(containerEngine.Containers(ContainerName)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not disappear after removal", ContainerName)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestEnsureAttachDetachRemove(t *testing.T) {
	containerEngine := testEngine(t)

	// Start from a clean slate but restore nothing: the cache is disposable by design.
	Remove(containerEngine)
	waitUntilGone(t, containerEngine)
	t.Cleanup(func() { Remove(containerEngine) })

	if !Ensure(containerEngine) {
		t.Skip("could not start the registry image (no network?)")
	}
	if containerEngine.Containers(ContainerName) == nil {
		t.Fatal("the mirror container was not created")
	}
	if !containerEngine.VolumeExists(VolumeName) {
		t.Error("the cache volume was not created")
	}
	if !containerEngine.NetworkExists(NetworkName) {
		t.Error("the cache network was not created")
	}

	// A second call must be a no-op rather than a second container.
	if !Ensure(containerEngine) {
		t.Error("Ensure was not idempotent")
	}
	running := 0
	for _, container := range containerEngine.Containers(ContainerName) {
		if container.Name == ContainerName {
			running++
		}
	}
	if running != 1 {
		t.Errorf("expected exactly one mirror container, found %d", running)
	}

	// The sandbox network is internal, so this connection is the only way the DinD daemon can
	// reach the mirror.
	sandboxNetwork := "hole-registry-itest-sandbox"
	_ = containerEngine.NetworkRemove(sandboxNetwork)
	if err := containerEngine.NetworkCreate(engine.NetworkOptions{
		Name:     sandboxNetwork,
		Subnet:   netip.MustParsePrefix("10.223.253.0/24"),
		Internal: true,
		Labels:   map[string]string{engine.LabelManaged: "true"},
	}); err != nil {
		t.Fatalf("create sandbox network: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.NetworkRemove(sandboxNetwork) })

	if !Attach(containerEngine, sandboxNetwork) {
		t.Fatal("could not attach the mirror to the sandbox network")
	}
	attached := containerEngine.NetworkAttachedContainers(sandboxNetwork)
	if len(attached) == 0 {
		t.Error("the mirror is not attached to the sandbox network")
	}

	Detach(containerEngine, sandboxNetwork)
	if len(containerEngine.NetworkAttachedContainers(sandboxNetwork)) != 0 {
		t.Error("the mirror is still attached after Detach")
	}
	// The network must now be removable, which is what teardown depends on.
	if err := containerEngine.NetworkRemove(sandboxNetwork); err != nil {
		t.Errorf("sandbox network could not be removed after Detach: %v", err)
	}

	Remove(containerEngine)
	if containerEngine.VolumeExists(VolumeName) {
		t.Error("the cache volume survived Remove")
	}
	if containerEngine.NetworkExists(NetworkName) {
		t.Error("the cache network survived Remove")
	}
}

// startProbeContainer runs a throwaway container from the pinned mirror image under the same
// restart policy Ensure uses, and returns its name. Both readiness cases below drive the real
// registry entrypoint rather than a stand-in, without needing an upstream: `serve` with a
// missing config file exits at once, and the default command serves locally and stays up.
func startProbeContainer(t *testing.T, containerEngine *engine.Engine, name string, args ...string) string {
	t.Helper()
	_ = containerEngine.ContainerRemove(name)
	runArgs := append([]string{"run", "-d", "--name", name, "--restart", restartPolicy, "--label",
		engine.LabelManaged + "=true", Image}, args...)
	if err := containerEngine.RunQuiet(runArgs...); err != nil {
		t.Skipf("could not start the registry image (no network?): %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(name) })
	return name
}

// TestWaitUntilServingRejectsAContainerThatDies is the regression for a mirror that `docker run
// -d` accepted and that then failed to come up: Ensure used to report it as usable and point the
// DinD daemon at a dead endpoint.
func TestWaitUntilServingRejectsAContainerThatDies(t *testing.T) {
	containerEngine := testEngine(t)
	name := startProbeContainer(t, containerEngine, "hole-registry-itest-dead",
		"serve", "/nonexistent-config.yml")

	start := time.Now()
	if waitUntilServing(containerEngine, name) {
		t.Error("a container that exits during startup was reported as a usable mirror")
	}
	if elapsed := time.Since(start); elapsed > readyTimeout {
		t.Errorf("the readiness probe took %s, longer than its own timeout %s", elapsed, readyTimeout)
	}
}

func TestWaitUntilServingAcceptsAContainerThatStaysUp(t *testing.T) {
	containerEngine := testEngine(t)
	name := startProbeContainer(t, containerEngine, "hole-registry-itest-alive")

	if !waitUntilServing(containerEngine, name) {
		logs, _ := containerEngine.ContainerLogs(name)
		t.Errorf("a running registry was not accepted as a usable mirror; logs:\n%s", logs)
	}
}

// TestEnsureRestartsAStoppedMirror also pins the round trip teardown depends on: the last
// sandbox to exit stops the mirror, and the next start must get the same cache back rather than
// a fresh container.
func TestEnsureRestartsAStoppedMirror(t *testing.T) {
	containerEngine := testEngine(t)
	Remove(containerEngine)
	waitUntilGone(t, containerEngine)
	t.Cleanup(func() { Remove(containerEngine) })

	if !Ensure(containerEngine) {
		t.Skip("could not start the registry image (no network?)")
	}
	Stop(containerEngine)
	if containerEngine.ContainerRunning(ContainerName) {
		t.Fatal("the mirror is still running after Stop")
	}
	if len(containerEngine.Containers(ContainerName)) == 0 {
		t.Fatal("Stop removed the mirror container instead of stopping it")
	}
	if !containerEngine.VolumeExists(VolumeName) {
		t.Error("Stop took the cache volume")
	}
	// Stopping an already stopped mirror must not warn or fail.
	Stop(containerEngine)

	// A stopped mirror is restarted rather than recreated, because its volume holds the cache.
	if !Ensure(containerEngine) {
		t.Fatal("Ensure did not bring the stopped mirror back")
	}
	if !containerEngine.ContainerRunning(ContainerName) {
		t.Error("the mirror is not running after Ensure")
	}
}
