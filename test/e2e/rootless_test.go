//go:build e2e && rootless

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/hostenv"
)

// This is intentionally a real `hole start`, not a simulated rootlesskit launch.
// Failure to meet prerequisites is a failure, not a skip. Run on a Docker host
// that permits unprivileged user namespaces and shares the test directories.
func TestRootlessAcceptance(t *testing.T) {
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("rootless acceptance requires a reachable Docker daemon: %v\n%s", err, out)
	}
	const image = "busybox:1.37.0"
	name := fmt.Sprintf("hole-rootless-acceptance-%d", time.Now().UnixNano())
	dockerOutput(t, "run", "-d", "--name", name, "-p", "0:8080", "-p", "0:8081", image,
		"sh", "-c", "mkdir -p /www; echo rootless-target > /www/index.html; httpd -p 8081 -h /www; exec httpd -f -p 8080 -h /www")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	port := dockerOutput(t, "inspect", "--format", `{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}`, name)
	blockedPort := dockerOutput(t, "inspect", "--format", `{{(index (index .NetworkSettings.Ports "8081/tcp") 0).HostPort}}`, name)
	home, project := environment(t, `["bash",".hole/acceptance.sh"]`, "")
	library, err := hostenv.ResolveProjectDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// macOS temp paths use /var -> /private/var. Hole mounts git-derived
	// libraries at their physical paths, so create the worktree and pass its
	// path to the sandbox using the same spelling (including Git metadata).
	worktreeParent, err := hostenv.ResolveProjectDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(worktreeParent, "linked")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", project}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("add", "README.md")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
	git("worktree", "add", "-b", "linked", worktree)
	settings := map[string]any{
		"container":   map[string]any{"docker": true, "dockerRootless": true, "agentUpdateCheck": false, "enabledAgents": []string{"test-agent"}},
		"libraries":   map[string]any{library: map[string]any{"path": "/rootless-library", "readwrite": true}},
		"git":         map[string]any{"worktreeLinks": "rw"},
		"environment": map[string]string{"TEST_WORKTREE": worktree, "TEST_PORT": port, "TEST_BLOCKED_PORT": blockedPort},
		"network":     map[string]any{"allow": []string{"docker.io"}, "hostGatewayDomains": []string{"rootless-target.invalid:" + port}},
		"hooks": map[string]any{
			"setup":     []map[string]string{{"script": ".hole/setup.sh"}},
			"setupHost": []map[string]string{{"script": ".hole/setup-host.sh"}},
			"prestart":  []map[string]string{{"script": ".hole/prestart.sh"}},
		},
	}
	raw, _ := json.Marshal(settings)
	write(t, filepath.Join(project, ".hole", "settings.json"), string(raw))
	write(t, filepath.Join(project, ".hole", "setup.sh"), "#!/bin/bash\nset -eu\ntest $(id -u) -ne 0\ntouch ~/rootless-setup-ran\n")
	// setupHost can inspect the outer runtime; prestart can inspect the nested API.
	// The daemon does not exist yet during setupHost, so audit it from the agent
	// by asking a background host observer via a project-local handshake.
	write(t, filepath.Join(project, ".hole", "setup-host.sh"), `#!/bin/bash
set -eu
test "$HOLE_DOCKER_ROOTLESS" = 1
printf '%s' "$HOLE_DOCKER_CONTAINER" > "$HOLE_PROJECT_DIR/sidecar-name"
touch "$HOLE_PROJECT_DIR/setup-host-ran"
`)
	write(t, filepath.Join(project, ".hole", "prestart.sh"), `#!/bin/bash
set -eu
test -f ~/rootless-setup-ran
test -f setup-host-ran
docker info --format '{{json .SecurityOptions}}' | grep -q name=rootless
touch prestart-ran
`)
	write(t, filepath.Join(project, ".hole", "acceptance.sh"), `#!/bin/bash
set -euo pipefail
sleep 3
test -f prestart-ran
docker info --format '{{json .SecurityOptions}}' | grep -q name=rootless
docker buildx version
docker compose version
docker run --rm busybox:1.37.0 true
mkdir -p .hole/build
printf 'FROM busybox:1.37.0\nRUN echo built > /built\nCMD ["cat", "/built"]\n' > .hole/build/Dockerfile
docker build -t rootless-build .hole/build
docker run --rm rootless-build | grep -qx built
docker buildx build --load -t rootless-buildx .hole/build
cat > .hole/compose.yaml <<'YAML'
services:
  test:
    image: rootless-buildx
    command: ["sleep", "120"]
YAML
docker compose -p acceptance -f .hole/compose.yaml up -d
docker compose -p acceptance -f .hole/compose.yaml exec -T test test -f /built
docker compose -p acceptance -f .hole/compose.yaml down
docker network create --internal acceptance-internal
docker run --rm --network acceptance-internal busybox:1.37.0 true
# Also leave an internal network and a running workload behind for teardown.
docker run -d --name teardown-probe --network acceptance-internal busybox:1.37.0 sleep 600
docker run --rm busybox:1.37.0 wget -T 10 -qO- "http://rootless-target.invalid:$TEST_PORT" | grep -q rootless-target
target_ip=$(getent ahostsv4 rootless-target.invalid | awk 'NR==1 {print $1}')
# Probe a known listening, but disallowed, port by literal IP. DNS denial alone
# cannot make this test pass. The fixture's other port is proven reachable above.
if docker run --rm busybox:1.37.0 wget -T 3 -qO- "http://$target_ip:$TEST_BLOCKED_PORT"; then
  echo 'disallowed L3/L4 egress succeeded' >&2; exit 1
fi
# slirp's host alias must not expose the outer sidecar's live API loopback port.
if docker run --rm --network host busybox:1.37.0 wget -T 3 -qO- http://10.0.2.2:2375/_ping; then
  echo 'slirp host loopback reachable' >&2; exit 1
fi
echo agent > /rootless-library/agent-wrote
docker run --rm -v /rootless-library:/library busybox:1.37.0 sh -c 'echo nested > /library/nested-wrote'
git -C "$TEST_WORKTREE" status --porcelain
echo linked > "$TEST_WORKTREE/agent-wrote"
docker run --rm -v "$TEST_WORKTREE:/linked" busybox:1.37.0 sh -c 'test -f /linked/.git; echo nested > /linked/nested-wrote'
touch ready-for-inspect
for i in $(seq 1 120); do
  if test -f inspected; then echo ROOTLESS_ACCEPTANCE_PASSED; exit 0; fi
  sleep 1
done
echo 'outer inspection did not complete' >&2
exit 1
`)
	// Observe while the CLI runs; avoid concurrent access to testing.T from a goroutine.
	cmd := exec.Command(holeBinary, "start", "test-agent", project, "--trust-project")
	cmd.Env = holeEnv(home)
	logPath := filepath.Join(t.TempDir(), "start.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(20 * time.Minute)
	inspected := false
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			out, _ := os.ReadFile(logPath)
			if err != nil || !inspected || !strings.Contains(string(out), "ROOTLESS_ACCEPTANCE_PASSED") {
				t.Fatalf("rootless start failed: %v\n%s", err, out)
			}
			for _, path := range []string{filepath.Join(library, "agent-wrote"), filepath.Join(library, "nested-wrote"), filepath.Join(worktree, "nested-wrote")} {
				if _, err := os.Stat(path); err != nil {
					t.Error(err)
				}
			}
			assertNoLeftovers(t, project)
			return
		default:
		}
		if !inspected {
			if _, err := os.Stat(filepath.Join(project, "ready-for-inspect")); err == nil {
				raw, err := os.ReadFile(filepath.Join(project, "sidecar-name"))
				if err != nil {
					t.Fatal(err)
				}
				sidecar := string(raw)
				if got := dockerOutput(t, "inspect", "--format", "{{.HostConfig.Privileged}} {{.Config.User}}", sidecar); strings.HasPrefix(got, "true ") || strings.HasSuffix(got, " 0") || strings.Contains(got, " root") {
					t.Fatalf("privileged sidecar: %s", got)
				}
				if got := dockerOutput(t, "exec", sidecar, "id", "-u"); got == "0" {
					t.Fatal("sidecar runs as root")
				}
				ns := dockerOutput(t, "exec", sidecar, "hole-dockerd-netns")
				if !strings.Contains(ns, "netns=net:[") {
					t.Fatalf("missing namespace inode: %s", ns)
				}
				// Enter the actual dockerd namespace; its internal bridge must exist here.
				id := dockerOutput(t, "exec", sidecar, "docker", "-H", "tcp://127.0.0.1:2375", "network", "inspect", "--format", "{{.Id}}", "acceptance-internal")
				dockerOutput(t, "exec", sidecar, "hole-dockerd-netns", "ip", "link", "show", "br-"+id[:12])
				write(t, filepath.Join(project, "inspected"), "yes")
				inspected = true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := os.ReadFile(logPath)
	t.Fatalf("rootless acceptance timed out\n%s", out)
}
