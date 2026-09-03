package assets

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBridgeNetfilterHelperShape pins the invariants of the helper that the Go side and the
// security argument depend on: the rule goes to the top of DOCKER-USER (the only chain Docker
// guarantees to evaluate before its own rules), is pinned to bridged frames, and the sweep
// only ever considers comments carrying Hole's own instance-name prefix.
func TestBridgeNetfilterHelperShape(t *testing.T) {
	script := string(readAsset(t, "gateway/hole-bridge-netfilter"))

	for _, marker := range []string{
		`-I DOCKER-USER 1`,
		`--physdev-is-bridged`,
		`COMMENT_PREFIX="hole-sandbox-"`,
		`HOLE_BRIDGE_NETFILTER=installed`,
		`HOLE_BRIDGE_NETFILTER=not-needed`,
		// A held xtables lock must degrade into a bounded wait, never an indefinite hang.
		`LOCK_WAIT_SECONDS=5`,
		// The sweep's liveness source is the bridge interface itself (global truth on a
		// shared daemon), never a per-user registry.
		`ip link show dev`,
	} {
		if !strings.Contains(script, marker) {
			t.Errorf("helper no longer contains %q", marker)
		}
	}

	dockerfile := string(readAsset(t, "gateway/Dockerfile"))
	for _, marker := range []string{
		"COPY hole-bridge-netfilter /usr/local/bin/hole-bridge-netfilter",
		"iptables \\",
		"command -v iptables-nft",
		"command -v iptables-legacy",
	} {
		if !strings.Contains(dockerfile, marker) {
			t.Errorf("gateway Dockerfile no longer contains %q — the helper would be missing "+
				"or unable to speak the host's iptables backend", marker)
		}
	}
}

// The behavioral tests run the real script against stub iptables binaries, so they need a
// bash with mapfile (4+); macOS's /bin/bash 3.2 skips.
func requireModernBash(t *testing.T) {
	t.Helper()
	out, err := exec.Command("bash", "-c", `echo ${BASH_VERSINFO[0]}`).Output()
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if major, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || major < 4 {
		t.Skipf("bash too old for the helper (need 4+, have %s)", strings.TrimSpace(string(out)))
	}
}

func readAsset(t *testing.T, path string) []byte {
	t.Helper()
	data, err := FS.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// helperHarness materializes the embedded script plus stub iptables backends and captures
// every stub invocation, one line per call, prefixed with the binary name.
type helperHarness struct {
	script     string
	logFile    string
	rulesFile  string
	sysctlFile string
	stubEnv    []string
}

func newHelperHarness(t *testing.T, sysctlValue string, existingRules, liveBridges []string, nftHasChain bool) *helperHarness {
	t.Helper()
	requireModernBash(t)
	dir := t.TempDir()

	harness := &helperHarness{
		script:     filepath.Join(dir, "hole-bridge-netfilter"),
		logFile:    filepath.Join(dir, "stub.log"),
		rulesFile:  filepath.Join(dir, "rules.txt"),
		sysctlFile: filepath.Join(dir, "bridge-nf-call-iptables"),
	}
	if err := os.WriteFile(harness.script, readAsset(t, "gateway/hole-bridge-netfilter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sysctlValue != "" {
		if err := os.WriteFile(harness.sysctlFile, []byte(sysctlValue+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(harness.rulesFile, []byte(strings.Join(existingRules, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bridgesFile := filepath.Join(dir, "bridges.txt")
	if err := os.WriteFile(bridgesFile, []byte(strings.Join(liveBridges, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stub answers the four invocation shapes the helper uses: chain existence probe,
	// rule listing, existence check (always "not present") and the mutations (always fine).
	stub := `#!/bin/bash
echo "$(basename "$0") $*" >> "$STUB_LOG"
args=" $* "
if [[ "$args" == *" --list DOCKER-USER "* ]]; then exit "${STUB_NO_CHAIN:-0}"; fi
if [[ "$args" == *" -S DOCKER-USER "* ]]; then cat "$STUB_RULES"; exit 0; fi
if [[ "$args" == *" -C DOCKER-USER "* ]]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "iptables-nft"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// The legacy backend never has the chain in these tests: one authoritative backend is
	// the common case, and it keeps the expected log unambiguous.
	if err := os.WriteFile(filepath.Join(dir, "iptables-legacy"),
		[]byte("#!/bin/bash\nif [[ \" $* \" == *\" --list DOCKER-USER \"* ]]; then exit 1; fi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// `ip link show dev <name>` answers from the fabricated live-bridge list — the sweep's
	// liveness source.
	ipStub := `#!/bin/bash
echo "ip $*" >> "$STUB_LOG"
grep -qx -- "${4:-}" "$STUB_BRIDGES"
`
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(ipStub), 0o755); err != nil {
		t.Fatal(err)
	}

	noChain := "0"
	if !nftHasChain {
		noChain = "1"
	}
	harness.stubEnv = []string{
		"PATH=" + dir + ":/usr/bin:/bin",
		"STUB_LOG=" + harness.logFile,
		"STUB_RULES=" + harness.rulesFile,
		"STUB_BRIDGES=" + bridgesFile,
		"STUB_NO_CHAIN=" + noChain,
		"HOLE_BRNF_SYSCTL_FILE=" + harness.sysctlFile,
	}
	return harness
}

func (h *helperHarness) run(t *testing.T, args ...string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command(h.script, args...)
	cmd.Env = h.stubEnv
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("helper did not run: %v (stderr: %s)", err, errOut.String())
		}
		code = exitErr.ExitCode()
	}
	t.Logf("helper %v: exit %d\nstdout: %sstderr: %s", args, code, out.String(), errOut.String())
	return out.String(), code
}

func (h *helperHarness) log(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.logFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestBridgeNetfilterHelperInstallsNothingOnHealthyHosts(t *testing.T) {
	for name, sysctl := range map[string]string{"sysctl absent": "", "sysctl zero": "0"} {
		t.Run(name, func(t *testing.T) {
			harness := newHelperHarness(t, sysctl, nil, nil, true)
			stdout, code := harness.run(t, "apply", "br-abcdef012345", "hole-sandbox-p-1")
			if code != 0 || !strings.Contains(stdout, "HOLE_BRIDGE_NETFILTER=not-needed") {
				t.Errorf("expected a clean not-needed answer, got exit %d, stdout %q", code, stdout)
			}
			log := harness.log(t)
			if strings.Contains(log, "-I DOCKER-USER") || strings.Contains(log, "-D DOCKER-USER") {
				t.Errorf("iptables was mutated on a host that does not filter bridged packets:\n%s", log)
			}
		})
	}
}

// A host that stopped filtering bridged packets (sysctl back to 0) must still collect rules
// left by sandboxes that died uncleanly while it did — otherwise they linger forever.
func TestBridgeNetfilterHelperSweepsEvenWhenNotNeeded(t *testing.T) {
	existing := []string{
		`-A DOCKER-USER -i br-deadbeef0001 -o br-deadbeef0001 -m physdev --physdev-is-bridged -m comment --comment hole-sandbox-dead-1111 -j ACCEPT`,
	}
	harness := newHelperHarness(t, "0", existing, nil, true)
	stdout, code := harness.run(t, "apply", "br-abcdef012345", "hole-sandbox-me-3333")
	if code != 0 || !strings.Contains(stdout, "HOLE_BRIDGE_NETFILTER=not-needed") {
		t.Fatalf("expected not-needed, got exit %d, stdout %q", code, stdout)
	}
	log := harness.log(t)
	if !strings.Contains(log, "-D DOCKER-USER -i br-deadbeef0001") {
		t.Errorf("the stale rule was not swept on the now-healthy host:\n%s", log)
	}
	if strings.Contains(log, "-I DOCKER-USER") {
		t.Errorf("a rule was installed although the sysctl is 0:\n%s", log)
	}
}

func TestBridgeNetfilterHelperInstallsAndSweeps(t *testing.T) {
	existing := []string{
		// Stale — its bridge no longer exists, so it must go.
		`-A DOCKER-USER -i br-deadbeef0001 -o br-deadbeef0001 -m physdev --physdev-is-bridged -m comment --comment hole-sandbox-dead-1111 -j ACCEPT`,
		// Live — its bridge exists (possibly another user's sandbox on a shared daemon, which
		// no registry could know about); quoted comment, as some iptables versions print it.
		`-A DOCKER-USER -i br-cafecafe0002 -o br-cafecafe0002 -m physdev --physdev-is-bridged -m comment --comment "hole-sandbox-live-2222" -j ACCEPT`,
		// Foreign — not Hole's, must never be considered.
		`-A DOCKER-USER -m comment --comment unrelated-rule -j RETURN`,
	}
	liveBridges := []string{"br-abcdef012345", "br-cafecafe0002"}
	harness := newHelperHarness(t, "1", existing, liveBridges, true)
	stdout, code := harness.run(t, "apply", "br-abcdef012345", "hole-sandbox-me-3333")
	if code != 0 || !strings.Contains(stdout, "HOLE_BRIDGE_NETFILTER=installed backend=iptables-nft physdev=yes") {
		t.Fatalf("expected installed with backend and physdev named, got exit %d, stdout %q", code, stdout)
	}

	log := harness.log(t)
	if !strings.Contains(log, "-D DOCKER-USER -i br-deadbeef0001") {
		t.Error("the stale rule was not swept")
	}
	if strings.Contains(log, "-D DOCKER-USER -i br-cafecafe0002") {
		t.Error("a rule whose bridge still exists was swept")
	}
	if strings.Contains(log, "unrelated-rule") && strings.Contains(log, "-D DOCKER-USER -m comment --comment unrelated-rule") {
		t.Error("a foreign rule was touched")
	}
	want := "-I DOCKER-USER 1 -i br-abcdef012345 -o br-abcdef012345 -m physdev --physdev-is-bridged " +
		"-m comment --comment hole-sandbox-me-3333 -j ACCEPT"
	if !strings.Contains(log, want) {
		t.Errorf("the rule was not inserted at the top of DOCKER-USER with the expected spec:\n%s", log)
	}
}

// Even when the liveness source claims no bridge exists at all, the rule belonging to the
// instance being started must survive the sweep.
func TestBridgeNetfilterHelperNeverSweepsItsOwnRule(t *testing.T) {
	existing := []string{
		`-A DOCKER-USER -i br-abcdef012345 -o br-abcdef012345 -m physdev --physdev-is-bridged -m comment --comment hole-sandbox-me-3333 -j ACCEPT`,
	}
	harness := newHelperHarness(t, "1", existing, nil, true)
	stdout, code := harness.run(t, "apply", "br-abcdef012345", "hole-sandbox-me-3333")
	if code != 0 || !strings.Contains(stdout, "HOLE_BRIDGE_NETFILTER=installed") {
		t.Fatalf("expected installed, got exit %d, stdout %q", code, stdout)
	}
	if log := harness.log(t); strings.Contains(log, "-D DOCKER-USER") {
		t.Errorf("the sweep deleted the rule of the instance being started:\n%s", log)
	}
}

func TestBridgeNetfilterHelperFailsClosedWithoutDockerUserChain(t *testing.T) {
	harness := newHelperHarness(t, "1", nil, nil, false)
	_, code := harness.run(t, "apply", "br-abcdef012345", "hole-sandbox-me-3333")
	if code != 3 {
		t.Errorf("an affected host with no DOCKER-USER chain must exit 3, got %d", code)
	}
	if log := harness.log(t); strings.Contains(log, "-I DOCKER-USER") {
		t.Errorf("nothing must be inserted when the chain is missing:\n%s", log)
	}
}

func TestBridgeNetfilterHelperRemovesOnlyItsOwnRule(t *testing.T) {
	existing := []string{
		`-A DOCKER-USER -i br-deadbeef0001 -o br-deadbeef0001 -m physdev --physdev-is-bridged -m comment --comment hole-sandbox-dead-1111 -j ACCEPT`,
		`-A DOCKER-USER -i br-cafecafe0002 -o br-cafecafe0002 -m physdev --physdev-is-bridged -m comment --comment hole-sandbox-live-2222 -j ACCEPT`,
	}
	harness := newHelperHarness(t, "1", existing, nil, true)
	if _, code := harness.run(t, "remove", "hole-sandbox-live-2222"); code != 0 {
		t.Fatalf("remove failed with exit %d", code)
	}
	log := harness.log(t)
	if !strings.Contains(log, "-D DOCKER-USER -i br-cafecafe0002") {
		t.Error("the targeted rule was not removed")
	}
	if strings.Contains(log, "-D DOCKER-USER -i br-deadbeef0001") {
		t.Error("remove deleted a rule belonging to another instance")
	}
}
