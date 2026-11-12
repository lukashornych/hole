//go:build integration

// Integration tests for the reliability layer: teardown, GC and the abandoned-instance
// paths. They fabricate the Docker resources a sandbox would own — networks, containers,
// volumes — instead of building sandbox images, so they exercise the real removal logic
// without a multi-minute image build. Run them with `make itest`.
package sandbox

import (
	"bytes"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/compose"
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
		instance.MaskVolume = maskVolumePrefix + instanceName
		if err := containerEngine.VolumeCreate(instance.MaskVolume, labels); err != nil {
			t.Fatalf("create mask volume: %v", err)
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
		if instance.MaskVolume != "" {
			_ = containerEngine.VolumeRemove(instance.MaskVolume)
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
	if containerEngine.VolumeExists(instance.MaskVolume) {
		t.Errorf("mask volume %s survived teardown", instance.MaskVolume)
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
	if !containerEngine.VolumeExists(instance.MaskVolume) {
		t.Error("GC removed the mask volume of a registered instance")
	}
	if _, err := os.Stat(instance.RunTmpDir); err != nil {
		t.Error("GC removed the run directory of a registered instance")
	}
}

func TestGCCollectsOrphanedVolume(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	// A volume whose instance has no state file, no network and no container is an orphan.
	orphans := []string{
		dindVolumePrefix + "hole-sandbox-itest-00000000-orph00",
		maskVolumePrefix + "hole-sandbox-itest-00000000-orph00",
	}
	for _, orphan := range orphans {
		if err := containerEngine.VolumeCreate(orphan, map[string]string{engine.LabelManaged: "true"}); err != nil {
			t.Fatalf("create volume: %v", err)
		}
		t.Cleanup(func() { _ = containerEngine.VolumeRemove(orphan) })
	}

	GC(containerEngine, host, store)

	for _, orphan := range orphans {
		if containerEngine.VolumeExists(orphan) {
			t.Errorf("orphaned volume %s was not collected", orphan)
		}
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

// Regression: startup used to write the state file before taking the liveness lock, and in that
// window (no lock, no watchdog yet) a concurrent `hole list` collected the starting sandbox.
func TestGCSparesAnInstanceThatHoldsItsLivenessBeforeTheWatchdogStarts(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)

	// The current order: liveness first, then the state write, watchdog not started yet.
	starting := fabricate(t, containerEngine, store, "early0", 140, false)
	release, err := store.HoldLiveness(starting.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	starting.WatchdogPID = 0
	starting.Phase = state.PhaseStarting
	if err := store.Write(starting); err != nil {
		t.Fatal(err)
	}

	// The old order: the state write without the lock. GC must see it as abandoned, or this test
	// would not cover the window at all.
	oldOrder := fabricate(t, containerEngine, store, "early1", 150, false)
	oldOrder.WatchdogPID = 0
	if err := store.Write(oldOrder); err != nil {
		t.Fatal(err)
	}

	GC(containerEngine, host, store)

	if !store.Exists(starting.InstanceName) {
		t.Error("GC deregistered an instance that holds its liveness lock")
	}
	if _, err := os.Stat(starting.RunTmpDir); err != nil {
		t.Error("GC removed the run directory of a starting instance")
	}
	for _, name := range starting.Networks {
		if !containerEngine.NetworkExists(name) {
			t.Errorf("GC removed network %s of a starting instance", name)
		}
	}
	if store.Exists(oldOrder.InstanceName) {
		t.Error("an instance registered without its liveness lock was not collected; the test no longer covers the window")
	}
}

func TestListNoGCLeavesAbandonedInstanceInPlace(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	// List detects the host itself, so point it at the test registry.
	t.Setenv("HOME", host.Home)

	abandoned := fabricate(t, containerEngine, store, "nogc00", 160, false)
	abandoned.CLIPID = deadPID
	abandoned.WatchdogPID = deadPID
	if err := store.Write(abandoned); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := List(&out, ListOptions{JSON: true, NoGC: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if out.String() != "[]\n" {
		t.Errorf("List output = %q, want an empty array: abandoned instances are never listed", out.String())
	}
	if !store.Exists(abandoned.InstanceName) {
		t.Error("List with NoGC collected the abandoned instance")
	}
	for _, name := range abandoned.Networks {
		if !containerEngine.NetworkExists(name) {
			t.Errorf("List with NoGC removed network %s", name)
		}
	}

	if err := List(io.Discard, ListOptions{JSON: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.Exists(abandoned.InstanceName) {
		t.Error("List without NoGC did not collect the abandoned instance")
	}
	for _, name := range abandoned.Networks {
		if containerEngine.NetworkExists(name) {
			t.Errorf("List without NoGC left network %s behind", name)
		}
	}
}

func TestTeardownMarksStoppingBeforeRemoving(t *testing.T) {
	containerEngine := testEngine(t)
	host, store := testHostAndStore(t)
	instance := fabricate(t, containerEngine, store, "stop00", 170, false)

	// cleanupHost runs late in teardown, so it sees what a polling caller sees meanwhile.
	seen := filepath.Join(instance.ProjectPath, "stopping-seen")
	stoppingMarker := filepath.Join(store.Dir(), instance.InstanceName+".stopping")
	script := "#!/bin/bash\n[ -e \"" + stoppingMarker + "\" ] && touch \"" + seen + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(instance.ProjectPath, "cleanup.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	instance.Settings = []byte(`{"hooks":{"cleanupHost":[{"script":"cleanup.sh"}]}}`)
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}

	Teardown(containerEngine, host, store, instance)

	if _, err := os.Stat(seen); err != nil {
		t.Error("the instance was not marked stopping while teardown ran")
	}
	if store.Stopping(instance.InstanceName) {
		t.Error("the stopping marker survived teardown")
	}
}

func TestStartRejectsATwinInstanceIDWithoutLeftovers(t *testing.T) {
	testEngine(t)
	host, store := testHostAndStore(t)
	// Start detects the host itself, so point it at the test registry.
	t.Setenv("HOME", host.Home)

	projectDir := t.TempDir()
	instanceName := hostenv.InstanceName(hostenv.ProjectName(projectDir), "twin00")
	// The twin holds the lock but has not written its state yet, so only the lock can reject.
	release, err := store.HoldLiveness(instanceName)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = Start(Options{Agent: "claude", ProjectDir: projectDir, InstanceID: "twin00"})
	if err == nil || !strings.Contains(err.Error(), "instance ID 'twin00' is already in use") {
		t.Fatalf("Start error = %v, want the instance ID to be rejected as in use", err)
	}
	if entries, err := os.ReadDir(host.TmpRoot()); err == nil && len(entries) > 0 {
		t.Errorf("a rejected start left %d run temp directories behind", len(entries))
	}
	if store.Exists(instanceName) {
		t.Error("a rejected start registered the instance")
	}
}

// Regression: the watchdog tore the sandbox down the instant the agent stopped, while the CLI was
// still on its way to attach. An agent that exits at once then had its container removed before
// the CLI could read the exit code, and `hole start` reported the attach client's 1 instead.
//
// Teardown's `compose down` never touches the plain `docker run` container used here, so the
// tests watch the registry: the stopping marker is the first thing Teardown drops, the
// deregistration the last.
func TestWatchdogKeepsAStoppedAgentUntilTheCLILetsGo(t *testing.T) {
	containerEngine := testEngine(t)

	tests := []struct {
		name   string
		suffix string
		octet  int
		// grace replaces agentStoppedGrace when set.
		grace time.Duration
		// letGo is what the CLI does once the hold window has passed; nil for a wedged CLI.
		letGo func(store *state.Store, instanceName string, release func())
	}{
		{
			name: "cli asks", suffix: "wdask0", octet: 180,
			letGo: func(store *state.Store, instanceName string, _ func()) { store.RequestTeardown(instanceName) },
		},
		{
			name: "cli dies", suffix: "wddie0", octet: 190,
			letGo: func(_ *state.Store, _ string, release func()) { release() },
		},
		{
			name: "cli wedged", suffix: "wdwdg0", octet: 210,
			grace: 4 * time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.grace > 0 {
				previous := agentStoppedGrace
				agentStoppedGrace = test.grace
				t.Cleanup(func() { agentStoppedGrace = previous })
			}

			host, store := testHostAndStore(t)
			instance := fabricate(t, containerEngine, store, test.suffix, test.octet, false)

			// The test plays the live CLI; flock is per open file description, so the
			// watchdog's in-process probe still finds the lock held.
			holdRelease, err := store.HoldLiveness(instance.InstanceName)
			if err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(holdRelease) }

			agentContainer := instance.InstanceName + "-agent-1"
			_ = containerEngine.ContainerRemove(agentContainer)
			if err := containerEngine.RunQuiet("run", "-d", "--name", agentContainer, "alpine:3.19", "true"); err != nil {
				release()
				t.Skipf("could not create a container: %v", err)
			}
			t.Cleanup(func() { _ = containerEngine.ContainerRemove(agentContainer) })
			if _, err := containerEngine.ContainerWait(agentContainer); err != nil {
				release()
				t.Skipf("container did not exit: %v", err)
			}

			// The watchdog's debug records are the only sign that it has seen the agent stop;
			// against a slow daemon its inspect and wait can outlast a fixed hold window.
			logFile := filepath.Join(t.TempDir(), "run.log")
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
				supervise(containerEngine, host, store, instance)
				close(done)
			}()
			// Runs before the container and resource cleanups, so a failing test never leaves
			// supervise tearing down resources that are already gone.
			t.Cleanup(func() {
				store.RequestTeardown(instance.InstanceName)
				release()
				select {
				case <-done:
				case <-time.After(60 * time.Second):
					t.Error("the watchdog did not return after the CLI let go")
				}
			})

			assertHeld := func() {
				t.Helper()
				select {
				case <-done:
					t.Fatal("the watchdog returned while the CLI still held the stopped agent")
				default:
				}
				if store.Stopping(instance.InstanceName) || !store.Exists(instance.InstanceName) {
					t.Fatal("the watchdog tore down a stopped agent before the CLI let go")
				}
			}
			seenDeadline := time.Now().Add(60 * time.Second)
			for {
				assertHeld()
				written, _ := os.ReadFile(logFile)
				if strings.Contains(string(written), "waiting for the CLI to read its exit code") {
					break
				}
				if time.Now().After(seenDeadline) {
					t.Fatal("the watchdog never noticed that the agent had stopped")
				}
				time.Sleep(50 * time.Millisecond)
			}
			holdUntil := time.Now().Add(3 * watchdogPoll)
			for time.Now().Before(holdUntil) {
				assertHeld()
				time.Sleep(50 * time.Millisecond)
			}

			if test.letGo != nil {
				test.letGo(store, instance.InstanceName, release)
			}

			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the watchdog did not tear down after the CLI let go")
			}
			if store.Exists(instance.InstanceName) {
				t.Error("the instance is still registered after the watchdog returned")
			}
		})
	}
}

// The agent container's init is what reaps orphans of the agent's tool calls and hooks; without
// one, the exec'd agent CLI is PID 1 and they stay zombies. The case without an init proves the
// probe sees the zombie at all; the case with one also proves the runtime can provide the init
// (podman needs catatonit installed for it).
func TestAgentContainerInitReapsOrphans(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull alpine: %v", err)
	}

	for i, withInit := range []bool{true, false} {
		project := "hole-sandbox-itest-00000000-init0" + itoa(i)
		projectDir := t.TempDir()
		// The inner shell exits at once, orphaning the subshell; the outer one then execs a
		// process that never reaps, exactly like the entrypoint's `exec "$@"`.
		data, err := compose.Marshal(&compose.File{Services: map[string]*compose.Service{
			"agent": {
				Image:   "alpine:3.19",
				Init:    withInit,
				Command: []string{"sh", "-c", "sh -c '(sleep 1; touch /tmp/orphan-exited) &'; exec sleep 300"},
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		composeFile := filepath.Join(projectDir, "compose.yaml")
		if err := os.WriteFile(composeFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		container := project + "-agent-1"
		// Without an init, `sleep` as PID 1 ignores SIGTERM: remove instead of sitting out the
		// stop grace period.
		t.Cleanup(func() {
			_ = containerEngine.ContainerRemove(container)
			_ = containerEngine.ComposeDown(project)
		})
		var output bytes.Buffer
		if err := containerEngine.ComposeUp(&output, project, composeFile, projectDir, false); err != nil {
			t.Fatalf("init=%t: compose up: %v\n%s", withInit, err, output.String())
		}

		if !eventually(func() bool {
			_, err := containerEngine.Exec(container, "test", "-f", "/tmp/orphan-exited")
			return err == nil
		}) {
			t.Fatalf("init=%t: the orphan never finished", withInit)
		}
		zombies := func() int {
			out, err := containerEngine.Exec(container, "sh", "-c", "cat /proc/[0-9]*/stat 2>/dev/null | awk '$3 == \"Z\"' | wc -l")
			if err != nil {
				t.Fatalf("init=%t: count zombies: %v", withInit, err)
			}
			count, err := strconv.Atoi(strings.TrimSpace(out))
			if err != nil {
				t.Fatalf("init=%t: zombie count %q: %v", withInit, out, err)
			}
			return count
		}
		if withInit && !eventually(func() bool { return zombies() == 0 }) {
			t.Errorf("the orphan is left a zombie although the container runs under an init")
		}
		// An omitted `init` falls back to the daemon's default, which can itself be an init.
		if !withInit && !eventually(func() bool { return zombies() > 0 }) {
			t.Log("no zombie without an init: the daemon likely runs containers under an init by default")
		}
	}
}

// eventually polls condition for a few seconds and reports whether it ever held.
func eventually(condition func() bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
