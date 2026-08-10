//go:build integration

// Integration tests for the container-runtime call sites. They need a real docker or
// podman daemon: run them with `make itest`.
package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/network"
)

const testLabelValue = "hole-engine-integration"

func testEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := Detect()
	if err != nil {
		t.Skipf("no container runtime available: %v", err)
	}
	return engine
}

func testLabels() map[string]string {
	return map[string]string{
		LabelManaged:  "true",
		LabelInstance: testLabelValue,
		LabelProject:  testLabelValue,
	}
}

func TestDetectReportsCompose(t *testing.T) {
	engine := testEngine(t)
	if engine.Binary != "docker" && engine.Binary != "podman" {
		t.Errorf("unexpected runtime %q", engine.Binary)
	}
}

func TestNetworkLifecycle(t *testing.T) {
	engine := testEngine(t)
	name := testLabelValue + "-lifecycle"
	subnet := netip.MustParsePrefix("10.223.240.0/24")

	_ = engine.NetworkRemove(name)
	if err := engine.NetworkCreate(NetworkOptions{Name: name, Subnet: subnet, Internal: true, Labels: testLabels()}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	t.Cleanup(func() { _ = engine.NetworkRemove(name) })

	if !engine.NetworkExists(name) {
		t.Fatal("created network is not visible")
	}

	networks, err := engine.Networks()
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	var found *NetworkInfo
	for i := range networks {
		if networks[i].Name == name {
			found = &networks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("created network missing from the inspect pass")
	}
	// Subnets and labels are what the allocator and GC rely on; both runtimes must report
	// them through this one code path.
	if len(found.Subnets) == 0 || found.Subnets[0] != subnet {
		t.Errorf("subnets = %v, want %v", found.Subnets, subnet)
	}
	if found.Labels[LabelManaged] != "true" {
		t.Errorf("labels = %v", found.Labels)
	}

	if err := engine.NetworkRemove(name); err != nil {
		t.Fatalf("NetworkRemove: %v", err)
	}
	if engine.NetworkExists(name) {
		t.Error("network still exists after removal")
	}
}

func TestNetworkCreateRejectsOverlappingSubnet(t *testing.T) {
	engine := testEngine(t)
	first := testLabelValue + "-overlap-a"
	second := testLabelValue + "-overlap-b"
	subnet := netip.MustParsePrefix("10.223.241.0/24")

	if err := engine.NetworkCreate(NetworkOptions{Name: first, Subnet: subnet, Labels: testLabels()}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	t.Cleanup(func() { _ = engine.NetworkRemove(first) })

	// Creation is atomic in the runtime, which is what makes the allocator's retry loop
	// correct under concurrent starts.
	if err := engine.NetworkCreate(NetworkOptions{Name: second, Subnet: subnet, Labels: testLabels()}); err == nil {
		_ = engine.NetworkRemove(second)
		t.Error("the runtime accepted an overlapping subnet")
	}
}

func TestAllocatorProducesUniqueSubnetsConcurrently(t *testing.T) {
	engine := testEngine(t)
	pool, err := network.ParsePool("10.224.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	const concurrency = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := map[string]string{}
	failures := make([]error, 0, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			name := fmt.Sprintf("%s-race-%d", testLabelValue, index)
			var lastErr error
			for attempt := 0; attempt < 10; attempt++ {
				infos, err := engine.Networks()
				if err != nil {
					lastErr = err
					continue
				}
				var used []netip.Prefix
				for _, info := range infos {
					used = append(used, info.Subnets...)
				}
				subnets, err := pool.Allocate(used, 1, attempt)
				if err != nil {
					lastErr = err
					continue
				}
				if err := engine.NetworkCreate(NetworkOptions{Name: name, Subnet: subnets[0], Labels: testLabels()}); err != nil {
					lastErr = err
					continue
				}
				mu.Lock()
				created[name] = subnets[0].String()
				mu.Unlock()
				return
			}
			mu.Lock()
			failures = append(failures, fmt.Errorf("%s: %w", name, lastErr))
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	t.Cleanup(func() {
		for name := range created {
			_ = engine.NetworkRemove(name)
		}
	})

	if len(failures) > 0 {
		t.Errorf("%d of %d concurrent allocations failed: %v", len(failures), concurrency, failures)
	}
	seen := map[string]string{}
	for name, subnet := range created {
		if other, clash := seen[subnet]; clash {
			t.Errorf("%s and %s got the same subnet %s", name, other, subnet)
		}
		seen[subnet] = name
	}
}

func TestVolumeLifecycle(t *testing.T) {
	engine := testEngine(t)
	name := testLabelValue + "-volume"
	_ = engine.VolumeRemove(name)

	if err := engine.VolumeCreate(name, testLabels()); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	t.Cleanup(func() { _ = engine.VolumeRemove(name) })

	if !engine.VolumeExists(name) {
		t.Fatal("created volume is not visible")
	}
	if names := engine.VolumesByName(testLabelValue); len(names) == 0 {
		t.Error("VolumesByName did not find the volume")
	}
	if err := engine.VolumeRemove(name); err != nil {
		t.Fatalf("VolumeRemove: %v", err)
	}
	if engine.VolumeExists(name) {
		t.Error("volume still exists after removal")
	}
}

func TestNetworkRemoveFailsWhileContainerAttached(t *testing.T) {
	engine := testEngine(t)
	name := testLabelValue + "-attached"
	subnet := netip.MustParsePrefix("10.223.242.0/24")
	_ = engine.NetworkRemove(name)

	if err := engine.NetworkCreate(NetworkOptions{Name: name, Subnet: subnet, Labels: testLabels()}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	container := testLabelValue + "-holder"
	if err := engine.RunQuiet("run", "-d", "--name", container, "--network", name,
		"alpine:3.19", "sh", "-c", "sleep 120"); err != nil {
		_ = engine.NetworkRemove(name)
		t.Skipf("could not start a holder container: %v", err)
	}
	t.Cleanup(func() {
		_ = engine.ContainerRemove(container)
		_ = engine.NetworkRemove(name)
	})

	if err := engine.NetworkRemove(name); err == nil {
		t.Error("removing a network with an attached container should fail so teardown can fall back")
	}
	// The teardown fallback removes whatever is still attached, then retries.
	attached := engine.NetworkAttachedContainers(name)
	if len(attached) == 0 {
		t.Fatal("NetworkAttachedContainers reported nothing to remove")
	}
	if err := engine.ContainerRemove(attached...); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	if err := engine.NetworkRemove(name); err != nil {
		t.Errorf("network removal still failed after the fallback: %v", err)
	}
}

// The watchdog decides "the agent is finished" from these two calls, and compose creates the
// agent container before starting it whenever it has to wait for the gateway's healthcheck.
// `wait` on a created container returns 0 immediately, so existence must not be read as having
// started: doing that tore sandboxes down mid-startup.
func TestContainerStartedDistinguishesCreatedFromStarted(t *testing.T) {
	engine := testEngine(t)
	container := testLabelValue + "-created"
	_ = engine.ContainerRemove(container)
	t.Cleanup(func() { _ = engine.ContainerRemove(container) })

	if err := engine.RunQuiet("create", "--name", container, "alpine:3.19", "sh", "-c", "sleep 120"); err != nil {
		t.Skipf("could not create a container: %v", err)
	}

	if !engine.ContainerExists(container) {
		t.Fatal("a created container must exist")
	}
	if engine.ContainerStarted(container) {
		t.Error("a created container must not count as started")
	}

	// The fact that makes the distinction load-bearing rather than cosmetic.
	waited := make(chan int, 1)
	go func() {
		code, _ := engine.ContainerWait(container)
		waited <- code
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("wait on a created container blocked — the watchdog's phase split assumes it returns at once")
	}

	if err := engine.RunQuiet("start", container); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !engine.ContainerStarted(container) {
		t.Error("a running container must count as started")
	}

	// kill rather than stop: `sleep` ignores SIGTERM, so a graceful stop costs the runtime's
	// full 10-second grace period for nothing.
	if err := engine.RunQuiet("kill", container); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Still "started" once it has exited, or a container that stops between two watchdog polls
	// would never be noticed.
	if !engine.ContainerStarted(container) {
		t.Error("an exited container must still count as started")
	}
}

// A failing healthcheck is all compose reports about a gateway that will not come up, so the
// probe output is the diagnostic. A container without a healthcheck must be silent rather than
// noisy: docker fails the inspect template outright there.
func TestContainerHealthProbeOutput(t *testing.T) {
	engine := testEngine(t)

	withHealthcheck := testLabelValue + "-unhealthy"
	withoutHealthcheck := testLabelValue + "-nohealth"
	_ = engine.ContainerRemove(withHealthcheck, withoutHealthcheck)
	t.Cleanup(func() { _ = engine.ContainerRemove(withHealthcheck, withoutHealthcheck) })

	if err := engine.RunQuiet("run", "-d", "--name", withHealthcheck,
		"--health-cmd", "echo probe-output-marker; exit 1",
		"--health-interval", "1s", "--health-retries", "1",
		"alpine:3.19", "sh", "-c", "sleep 120"); err != nil {
		t.Skipf("could not start a container with a healthcheck: %v", err)
	}
	if err := engine.RunQuiet("run", "-d", "--name", withoutHealthcheck,
		"alpine:3.19", "sh", "-c", "sleep 120"); err != nil {
		t.Skipf("could not start a container: %v", err)
	}

	var probe string
	for attempt := 0; attempt < 20 && probe == ""; attempt++ {
		time.Sleep(500 * time.Millisecond)
		probe = engine.ContainerHealthProbeOutput(withHealthcheck)
	}
	if !strings.Contains(probe, "probe-output-marker") {
		t.Errorf("probe output = %q, want it to carry the healthcheck's own output", probe)
	}
	if probe := engine.ContainerHealthProbeOutput(withoutHealthcheck); probe != "" {
		t.Errorf("a container without a healthcheck reported %q", probe)
	}
	if probe := engine.ContainerHealthProbeOutput(testLabelValue + "-does-not-exist"); probe != "" {
		t.Errorf("a missing container reported %q", probe)
	}
}

// Attaching to a TTY-enabled container is what `hole start` ends with, and a runtime refuses a
// non-terminal stdin on one. `go test` supplies exactly that, so this reproduces the condition
// under which every non-interactive run used to fail with exit 1 instead of the agent's status.
func TestAttachWithoutATerminal(t *testing.T) {
	engine := testEngine(t)
	if IsTerminal(os.Stdin) {
		t.Skip("stdin is a terminal; this test covers the non-interactive path")
	}

	tests := []struct {
		name     string
		runFlags []string
	}{
		// What a non-interactive `hole start` now generates: no TTY, no open stdin, so the
		// agent's process reads EOF and exits rather than waiting for input.
		{name: "plain"},
		// A TTY-enabled container is what an interactive run generates. Attaching to one
		// without a terminal is what the runtime refuses outright, so the guard has to hold
		// here too.
		{name: "tty enabled", runFlags: []string{"-it"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			container := testLabelValue + "-attach-" + strings.ReplaceAll(test.name, " ", "-")
			_ = engine.ContainerRemove(container)
			t.Cleanup(func() { _ = engine.ContainerRemove(container) })

			args := append([]string{"run", "-d"}, test.runFlags...)
			args = append(args, "--name", container, "alpine:3.19", "sh", "-c", "sleep 1; exit 7")
			if err := engine.RunQuiet(args...); err != nil {
				t.Skipf("could not start a container: %v", err)
			}

			err := engine.Attach(container)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("Attach err = %v, want the container's exit status", err)
			}
			if exitErr.ExitCode() != 7 {
				t.Errorf("Attach exit code = %d, want 7 (the container's own)", exitErr.ExitCode())
			}
		})
	}
}
