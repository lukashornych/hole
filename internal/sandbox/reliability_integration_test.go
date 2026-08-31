//go:build integration

// Integration tests for the reliability layer: teardown, GC and the abandoned-instance
// paths. They fabricate the Docker resources a sandbox would own — networks, containers,
// volumes — instead of building sandbox images, so they exercise the real removal logic
// without a multi-minute image build. Run them with `make itest`.
package sandbox

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

const (
	// deadPID cannot be a live process, so instances using it look abandoned.
	deadPID = -424242
	// testSubnetBase is a range unlikely to collide with anything else on the host.
	testSubnetBase = "10.225."
)

func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	containerEngine, err := engine.Detect()
	if err != nil {
		t.Skipf("no container runtime available: %v", err)
	}
	return containerEngine
}

func testHostAndStore(t *testing.T) (hostenv.Host, *state.Store) {
	t.Helper()
	host := hostenv.Host{Home: t.TempDir(), Username: "tester"}
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}
	return host, store
}

// fabricate registers an instance and creates the Docker resources it would own.
func fabricate(t *testing.T, containerEngine *engine.Engine, store *state.Store, suffix string, octet int, withVolume bool) *state.Instance {
	t.Helper()
	instanceName := "hole-sandbox-itest-00000000-" + suffix
	instance := &state.Instance{
		InstanceName: instanceName,
		InstanceID:   suffix,
		ProjectPath:  t.TempDir(),
		ProjectName:  "itest-00000000",
		Agent:        "test-agent",
		CLIPID:       os.Getpid(),
		WatchdogPID:  os.Getpid(),
		Networks:     []string{instanceName + "_sandbox", instanceName + "_internet"},
		RunTmpDir:    filepath.Join(t.TempDir(), "run.itest"),
		StartedAt:    time.Now(),
	}
	if err := os.MkdirAll(instance.RunTmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	labels := resourceLabels(instanceName, instance.ProjectName)
	for i, name := range instance.Networks {
		subnet := netip.MustParsePrefix(testSubnetBase + itoa(octet+i) + ".0/24")
		_ = containerEngine.NetworkRemove(name)
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: name, Subnet: subnet, Internal: i == 0, Labels: labels,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if withVolume {
		instance.DinDEnabled = true
		instance.DinDVolume = dindVolumePrefix + instanceName
		if err := containerEngine.VolumeCreate(instance.DinDVolume, labels); err != nil {
			t.Fatalf("create volume: %v", err)
		}
	}
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, name := range instance.Networks {
			_ = containerEngine.NetworkRemove(name)
		}
		if instance.DinDVolume != "" {
			_ = containerEngine.VolumeRemove(instance.DinDVolume)
		}
		store.Remove(instanceName)
	})
	return instance
}

func itoa(value int) string {
	digits := "0123456789"
	if value == 0 {
		return "0"
	}
	var out []byte
	for value > 0 {
		out = append([]byte{digits[value%10]}, out...)
		value /= 10
	}
	return string(out)
}

func TestTeardownRemovesEverythingItOwns(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "tdown0", 10, true)

	Teardown(containerEngine, host, store, instance)

	for _, name := range instance.Networks {
		if containerEngine.NetworkExists(name) {
			t.Errorf("network %s survived teardown", name)
		}
	}
	if containerEngine.VolumeExists(instance.DinDVolume) {
		t.Errorf("volume %s survived teardown", instance.DinDVolume)
	}
	if store.Exists(instance.InstanceName) {
		t.Error("the instance is still registered after teardown")
	}
	if _, err := os.Stat(instance.RunTmpDir); err == nil {
		t.Error("the run directory survived teardown")
	}
}

func TestTeardownIsIdempotent(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "tdown1", 20, true)

	Teardown(containerEngine, host, store, instance)
	// A second pass is what happens when the CLI falls back after a watchdog that had
	// already finished; it must be silent and harmless.
	Teardown(containerEngine, host, store, instance)

	if store.Exists(instance.InstanceName) {
		t.Error("the instance is still registered")
	}
}

func TestTeardownRemovesNetworkWithAttachedContainer(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "tdown2", 30, false)

	// A partial `compose down` can leave a container attached; teardown must force it out
	// rather than leave the network behind. This is the case the bash version failed on.
	holder := instance.InstanceName + "-holder"
	if err := containerEngine.RunQuiet("run", "-d", "--name", holder,
		"--network", instance.Networks[1], "alpine:3.19", "sleep", "120"); err != nil {
		t.Skipf("could not start a holder container: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(holder) })

	Teardown(containerEngine, host, store, instance)

	for _, name := range instance.Networks {
		if containerEngine.NetworkExists(name) {
			t.Errorf("network %s survived teardown despite the attached-container fallback", name)
		}
	}
}

func TestTeardownRunsCleanupHostFromTheSettingsSnapshot(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "tdown3", 40, false)

	marker := filepath.Join(instance.ProjectPath, "cleanup-ran")
	scriptPath := filepath.Join(instance.ProjectPath, "cleanup.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	instance.Settings = []byte(`{"hooks":{"cleanupHost":[{"script":"cleanup.sh"}]}}`)
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}

	Teardown(containerEngine, host, store, instance)

	if _, err := os.Stat(marker); err != nil {
		t.Error("cleanupHost hook from the settings snapshot did not run")
	}
}

func TestGCTearsDownAbandonedInstances(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	abandoned := fabricate(t, containerEngine, store, "aband0", 50, true)
	healthy := fabricate(t, containerEngine, store, "live00", 60, true)

	// Both processes gone: this is the `kill -9` of CLI *and* watchdog that only GC covers.
	abandoned.CLIPID = deadPID
	abandoned.WatchdogPID = deadPID
	if err := store.Write(abandoned); err != nil {
		t.Fatal(err)
	}

	GC(containerEngine, host, store)

	if store.Exists(abandoned.InstanceName) {
		t.Error("the abandoned instance was not collected")
	}
	for _, name := range abandoned.Networks {
		if containerEngine.NetworkExists(name) {
			t.Errorf("network %s of the abandoned instance survived GC", name)
		}
	}
	if containerEngine.VolumeExists(abandoned.DinDVolume) {
		t.Error("the abandoned instance's volume survived GC")
	}

	// A live instance must be untouched — that distinction is the whole point of the registry.
	if !store.Exists(healthy.InstanceName) {
		t.Error("GC deregistered a healthy instance")
	}
	for _, name := range healthy.Networks {
		if !containerEngine.NetworkExists(name) {
			t.Errorf("GC removed network %s of a healthy instance", name)
		}
	}
	if !containerEngine.VolumeExists(healthy.DinDVolume) {
		t.Error("GC removed the volume of a healthy instance")
	}
}

func TestGCKeepsRegisteredInstanceResources(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "keep00", 70, true)

	// A freshly created network has not aged past the grace period, so even the label-based
	// prune must leave it alone.
	GC(containerEngine, host, store)

	for _, name := range instance.Networks {
		if !containerEngine.NetworkExists(name) {
			t.Errorf("GC removed network %s of a registered instance", name)
		}
	}
	if !containerEngine.VolumeExists(instance.DinDVolume) {
		t.Error("GC removed the volume of a registered instance")
	}
	if _, err := os.Stat(instance.RunTmpDir); err != nil {
		t.Error("GC removed the run directory of a registered instance")
	}
}

func TestGCCollectsOrphanedVolume(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	// A volume whose instance has no state file, no network and no container is an orphan.
	orphan := dindVolumePrefix + "hole-sandbox-itest-00000000-orph00"
	if err := containerEngine.VolumeCreate(orphan, map[string]string{engine.LabelManaged: "true"}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(orphan) })

	GC(containerEngine, host, store)

	if containerEngine.VolumeExists(orphan) {
		t.Error("orphaned Docker-in-Docker volume was not collected")
	}
}

func TestGCCollectsStoppedContainersOfGoneInstance(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	// Shaped exactly like compose names it, with no state file and no networks left.
	container := "hole-sandbox-itest-00000000-stop00-agent-1"
	if err := containerEngine.RunQuiet("run", "-d", "--name", container, "alpine:3.19", "true"); err != nil {
		t.Skipf("could not create a container: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	// Wait for it to exit so GC sees a stopped container.
	if _, err := containerEngine.ContainerWait(container); err != nil {
		t.Skipf("container did not exit: %v", err)
	}

	GC(containerEngine, host, store)

	if containerEngine.ContainerExists(container) {
		t.Error("stopped container of a gone instance was not collected")
	}
}

func TestGCKeepsRunningContainerOfGoneInstance(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	// Running containers are never reaped by the container pass: only the abandoned-instance
	// pass may do that, and only when a state file proves nobody owns them.
	container := "hole-sandbox-itest-00000000-runn00-agent-1"
	if err := containerEngine.RunQuiet("run", "-d", "--name", container, "alpine:3.19", "sleep", "120"); err != nil {
		t.Skipf("could not create a container: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	GC(containerEngine, host, store)

	if !containerEngine.ContainerExists(container) {
		t.Error("GC removed a running container")
	}
}

func TestGCIgnoresUnrelatedResources(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	network := "itest-unrelated-network"
	_ = containerEngine.NetworkRemove(network)
	if err := containerEngine.NetworkCreate(engine.NetworkOptions{
		Name: network, Subnet: netip.MustParsePrefix(testSubnetBase + "200.0/24"),
	}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.NetworkRemove(network) })

	volume := "itest-unrelated-volume"
	if err := containerEngine.VolumeCreate(volume, nil); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(volume) })

	GC(containerEngine, host, store)

	if !containerEngine.NetworkExists(network) {
		t.Error("GC removed an unlabeled network that is not Hole's")
	}
	if !containerEngine.VolumeExists(volume) {
		t.Error("GC removed a volume that is not Hole's")
	}
}

// The CLI stops relaying the watchdog's output the instant the instance leaves the registry, so
// anything Teardown logs after deregistering reaches the log file but never the user's console.
// The closing "Sandbox destroyed" line was in exactly that position and went missing whenever the
// CLI's last check won the race.
//
// One-directional by construction: with the ordering correct this always passes, because the log
// handler is unbuffered and the message is written strictly before the removal.
func TestTeardownDeregistersOnlyAfterItsLastMessage(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "order0", 100, false)

	logFile := filepath.Join(t.TempDir(), "run.log")
	instance.LogFile = logFile
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}

	// An agent container that ran and exited: that is what makes the closing message appear.
	agentContainer := instance.InstanceName + "-agent-1"
	if err := containerEngine.RunQuiet("run", "-d", "--name", agentContainer, "alpine:3.19", "true"); err != nil {
		t.Skipf("could not create a container: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(agentContainer) })
	if _, err := containerEngine.ContainerWait(agentContainer); err != nil {
		t.Skipf("container did not exit: %v", err)
	}

	restoreLogging, err := logging.Setup(logging.Options{LogFile: logFile, Quiet: true, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restoreLogging()
		// Leave the package logger writing nowhere rather than to a closed file.
		if reset, err := logging.Setup(logging.Options{Quiet: true}); err == nil {
			_ = reset
		}
	})

	done := make(chan struct{})
	go func() {
		Teardown(containerEngine, host, store, instance)
		close(done)
	}()

	// Poll exactly as the CLI does, and read the log at the first moment it would return.
	deadline := time.Now().Add(30 * time.Second)
	for store.Exists(instance.InstanceName) {
		if time.Now().After(deadline) {
			t.Fatal("teardown did not deregister the instance")
		}
	}
	written, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "Sandbox destroyed") {
		t.Error("the instance was deregistered before the closing message was logged — the CLI would drop it")
	}
	<-done
}

func TestTeardownRefreshesStaleInstanceSnapshot(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "stale0", 80, true)

	// The watchdog reads the registry when it starts — before startup has recorded the
	// networks and the volume. Tearing down from that stale view must still remove them.
	stale := &state.Instance{
		InstanceName: instance.InstanceName,
		InstanceID:   instance.InstanceID,
		ProjectPath:  instance.ProjectPath,
		ProjectName:  instance.ProjectName,
		Agent:        instance.Agent,
		CLIPID:       instance.CLIPID,
		WatchdogPID:  instance.WatchdogPID,
	}

	Teardown(containerEngine, host, store, stale)

	for _, name := range instance.Networks {
		if containerEngine.NetworkExists(name) {
			t.Errorf("network %s survived a teardown driven by a stale snapshot", name)
		}
	}
	if containerEngine.VolumeExists(instance.DinDVolume) {
		t.Errorf("volume %s survived a teardown driven by a stale snapshot", instance.DinDVolume)
	}
	if store.Exists(instance.InstanceName) {
		t.Error("the instance is still registered")
	}
}

// An agent whose command finishes before the attach lands makes the runtime refuse with
// "cannot attach to a stopped container" and exit 1. Reporting that as the run's exit code
// turns a successful agent into a failed `hole start`.
func TestAttachReportsTheContainersStatusNotTheAttachClients(t *testing.T) {
	containerEngine := testEngine(t)

	tests := []struct {
		name     string
		command  string
		wantCode int
	}{
		{name: "already exited cleanly", command: "true", wantCode: 0},
		{name: "already exited with a failure", command: "exit 3", wantCode: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			container := "hole-sandbox-attach-status-" + strings.ReplaceAll(test.name, " ", "-")
			_ = containerEngine.ContainerRemove(container)
			t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

			if err := containerEngine.RunQuiet("run", "-d", "--name", container,
				"alpine:3.19", "sh", "-c", test.command); err != nil {
				t.Skipf("could not start a container: %v", err)
			}
			// Attach only once it is unambiguously gone: that is the race being pinned.
			for attempt := 0; attempt < 40 && containerEngine.ContainerRunning(container); attempt++ {
				time.Sleep(100 * time.Millisecond)
			}
			if containerEngine.ContainerRunning(container) {
				t.Skip("the container did not exit in time")
			}

			if code := attach(containerEngine, container); code != test.wantCode {
				t.Errorf("attach reported %d, want %d (the container's own status)", code, test.wantCode)
			}
		})
	}
}
