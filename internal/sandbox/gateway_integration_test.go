//go:build integration

// Integration tests for the gateway entrypoint itself: they build the real gateway image from
// the embedded assets and start it with a fabricated /etc/hosts, which is the only way to
// reproduce how a container runtime hands the host gateway address over. Run them with
// `make itest`.
//
// The generated configuration is copied into the container rather than bind-mounted, so the
// tests also pass against a remote daemon, where a bind mount would silently resolve to an
// empty directory.
package sandbox

import (
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/assets"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/image"
	"github.com/lukashornych/hole/v2/internal/network"
)

// hostGatewayTestAddress is in TEST-NET-1, so it can never collide with a real host gateway and
// is distinguishable from the loopback address the health zone legitimately answers.
const hostGatewayTestAddress = "192.0.2.77"

// gatewayTestContext builds the gateway image and materializes the artifacts a policy with one
// `hostGatewayDomains` entry generates — the only policy shape that puts `{HOST_GATEWAY_IP}`
// into the Corefile, and therefore the only one that arms the entrypoint's check.
func gatewayTestContext(t *testing.T, containerEngine *engine.Engine) (gatewayImage, confDir string) {
	t.Helper()
	runTmpDir := t.TempDir()
	policy := network.BuildPolicy(nil,
		[]network.HostGatewayDomain{{Domain: "mydb.local", Ports: []int{5432}}}, false)
	confDir, err := writeGatewayArtifacts(runTmpDir, policy)
	if err != nil {
		t.Fatalf("writeGatewayArtifacts: %v", err)
	}

	gatewayImage = image.GatewayImage(assets.BuildInputsHash())
	if !containerEngine.ImageExists(gatewayImage) {
		// The build pulls CoreDNS from GitHub and packages from the Ubuntu mirrors, which the
		// agent sandbox cannot reach; skipping there beats failing for an unrelated reason.
		if err := containerEngine.RunQuiet("build", "-t", gatewayImage,
			filepath.Join(runTmpDir, "gateway")); err != nil {
			t.Skipf("could not build the gateway image (the build needs network access): %v", err)
		}
	}
	return gatewayImage, confDir
}

// startGatewayContainer creates the container, copies the generated configuration in and starts
// it. addHosts entries are passed through verbatim, in order, which is what decides the order of
// the /etc/hosts lines the entrypoint reads.
func startGatewayContainer(t *testing.T, containerEngine *engine.Engine,
	container, gatewayImage, confDir, subnet string, networks []string, addHosts ...string) {

	t.Helper()
	args := []string{"create", "--name", container,
		"--cap-add", "NET_ADMIN",
		"--sysctl", "net.ipv4.ip_forward=1",
		"--label", engine.LabelManaged + "=true",
		"--label", engine.LabelInstance + "=" + container,
		"--label", engine.LabelProject + "=itest-gateway",
		"-e", "HOLE_SANDBOX_SUBNET=" + subnet,
	}
	for _, addHost := range addHosts {
		args = append(args, "--add-host", addHost)
	}
	if len(networks) > 0 {
		args = append(args, "--network", networks[0])
	}
	args = append(args, gatewayImage)
	if err := containerEngine.RunQuiet(args...); err != nil {
		t.Skipf("could not create the gateway container: %v", err)
	}
	if len(networks) > 1 {
		for _, name := range networks[1:] {
			if err := containerEngine.NetworkConnect(name, container); err != nil {
				t.Fatalf("connect %s: %v", name, err)
			}
		}
	}
	// docker cp creates /etc/hole from the source directory, so the container needs no volume.
	if err := containerEngine.RunQuiet("cp", confDir, container+":/etc/hole"); err != nil {
		t.Fatalf("copy the generated configuration into the container: %v", err)
	}
	if err := containerEngine.RunQuiet("start", container); err != nil {
		t.Fatalf("start %s: %v", container, err)
	}
}

// waitForGatewayLog polls the container log for a marker, which is how both tests observe the
// entrypoint without waiting for a healthcheck that may never pass.
func waitForGatewayLog(t *testing.T, containerEngine *engine.Engine, container, marker string) string {
	t.Helper()
	var logs string
	for attempt := 0; attempt < 40; attempt++ {
		logs, _ = containerEngine.ContainerLogs(container)
		if strings.Contains(logs, marker) {
			return logs
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the gateway never logged %q; log was:\n%s", marker, logs)
	return logs
}

// TestGatewayFailsOnLoopbackHostGateway covers the fail-loudly half of the fix: a runtime that
// offers nothing but a loopback address for the host gateway must abort startup rather than
// substitute it, which used to make every configured name resolve to the sandbox itself.
//
// One network is enough — the check precedes interface discovery.
func TestGatewayFailsOnLoopbackHostGateway(t *testing.T) {
	containerEngine := testEngine(t)
	gatewayImage, confDir := gatewayTestContext(t, containerEngine)

	container := "hole-sandbox-itest-gateway-loopback"
	_ = containerEngine.ContainerRemove(container)
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	startGatewayContainer(t, containerEngine, container, gatewayImage, confDir,
		"10.225.90.0/24", nil, "host.internal:127.0.0.1")

	code, err := containerEngine.ContainerWait(container)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code == 0 {
		logs, _ := containerEngine.ContainerLogs(container)
		t.Fatalf("the gateway started with a loopback host gateway address; log was:\n%s", logs)
	}
	logs, _ := containerEngine.ContainerLogs(container)
	if !strings.Contains(logs, "no usable IPv4 host gateway address") {
		t.Errorf("the abort did not name its cause; log was:\n%s", logs)
	}
}

// TestGatewayPrefersIPv4HostGateway is the regression itself: with both an IPv4 and an IPv6
// entry for the same name — IPv6 first, as OrbStack writes them — the entrypoint has to pick the
// IPv4 one. `getent hosts` answers AF_INET6 first, so the old lookup found nothing here and fell
// back to 127.0.0.1.
//
// The address the entrypoint logs is the exact value it substitutes into both generated files,
// so asserting on the log needs no exec into a container that may still be starting.
func TestGatewayPrefersIPv4HostGateway(t *testing.T) {
	containerEngine := testEngine(t)
	gatewayImage, confDir := gatewayTestContext(t, containerEngine)

	container := "hole-sandbox-itest-gateway-dualstack"
	_ = containerEngine.ContainerRemove(container)
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	// Two networks, because interface discovery demands one address inside the sandbox subnet
	// and one outside it; the entrypoint logs the host gateway address only after that succeeds.
	subnets := []string{"10.225.91.0/24", "10.225.92.0/24"}
	networks := []string{container + "-sandbox", container + "-internet"}
	labels := resourceLabels(container, "itest-gateway")
	for i, name := range networks {
		_ = containerEngine.NetworkRemove(name)
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: name, Subnet: netip.MustParsePrefix(subnets[i]), Internal: i == 0, Labels: labels,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = containerEngine.NetworkRemove(name) })
	}

	startGatewayContainer(t, containerEngine, container, gatewayImage, confDir, subnets[0], networks,
		"host.internal:fd07:b51a:cc66:f0::fe", "host.internal:"+hostGatewayTestAddress)

	logs := waitForGatewayLog(t, containerEngine, container, "host gateway=")
	if !strings.Contains(logs, "host gateway="+hostGatewayTestAddress) {
		t.Errorf("the gateway did not use the IPv4 host gateway address; log was:\n%s", logs)
	}
}
