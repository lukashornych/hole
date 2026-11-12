package assets

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedTreeIsComplete(t *testing.T) {
	required := []string{
		"agents/Dockerfile",
		"agents/Dockerfile.update",
		"agents/entrypoint.sh",
		"agents/claude/command.json",
		"agents/claude/allow.txt",
		"agents/claude/update-user.sh",
		"agents/claude/version.json",
		"agents/gemini/command.json",
		"agents/gemini/allow.txt",
		"agents/gemini/update-user.sh",
		"agents/gemini/version.json",
		"agents/codex/command.json",
		"agents/codex/allow.txt",
		"agents/codex/update-user.sh",
		"agents/codex/version.json",
		"gateway/Dockerfile",
		"gateway/entrypoint.sh",
		"gateway/hole-bridge-netfilter",
		"gateway/hole-gateway-apply",
		"gateway/hole-gateway-lib.sh",
		"schema/settings.schema.json",
	}
	for _, path := range required {
		if _, err := fs.Stat(FS, path); err != nil {
			t.Errorf("required asset %s is not embedded: %v", path, err)
		}
	}
}

func TestBuildInputsHashIsStable(t *testing.T) {
	first := BuildInputsHash()
	for i := 0; i < 5; i++ {
		if BuildInputsHash() != first {
			t.Fatal("BuildInputsHash is not stable within a build")
		}
	}
	if len(first) != 40 {
		t.Errorf("BuildInputsHash = %q, want a sha1 hex digest", first)
	}
}

// TestGatewayDockerfileVerifiesCoreDNS keeps the DNS policy engine from being installed on
// nothing but TLS: the tarball must be downloaded to a file and checksummed, never piped
// straight into tar, and every architecture must have a real sum to check it against.
func TestGatewayDockerfileVerifiesCoreDNS(t *testing.T) {
	data, err := FS.ReadFile("gateway/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)

	if !strings.Contains(dockerfile, "sha256sum --check") {
		t.Error("the CoreDNS download is not checksum-verified")
	}
	if regexp.MustCompile(`coredns\S*\.tgz"?\s*\\?\s*\|\s*tar`).MatchString(dockerfile) {
		t.Error("the CoreDNS tarball is piped into tar, so nothing can verify it first")
	}
	for _, arch := range []string{"AMD64", "ARM64"} {
		pattern := regexp.MustCompile(`ARG COREDNS_SHA256_` + arch + `=([0-9a-f]{64})\b`)
		if !pattern.MatchString(dockerfile) {
			t.Errorf("COREDNS_SHA256_%s is missing or is not a sha256 digest", arch)
		}
	}
}

// TestGatewayEntrypointDropsForwardingFirst pins the fail-closed startup order: the container is
// already forwarding when the entrypoint begins, so the default-drop table has to be installed
// before any check that can block or abort, and long before the generated ruleset arrives.
func TestGatewayEntrypointDropsForwardingFirst(t *testing.T) {
	data, err := FS.ReadFile("gateway/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(data)

	dropChain := regexp.MustCompile(`(?s)nft -f -.*?chain forward \{.*?policy drop;`)
	dropIndex := dropChain.FindStringIndex(entrypoint)
	if dropIndex == nil {
		t.Fatal("the entrypoint does not install a default-drop forward chain before setup")
	}
	rulesetIndex := strings.Index(entrypoint, `nft -f /tmp/hole/nftables.rules`)
	if rulesetIndex < 0 {
		t.Fatal("the entrypoint no longer applies the generated ruleset")
	}
	// Any check that can abort or block must sit behind the drop, so the two markers below stand
	// in for "the rest of startup".
	for _, marker := range []string{"source /usr/local/bin/hole-gateway-lib.sh", "dnsmasq --version",
		"host_gateway_address", "ip -o -4 addr show"} {
		markerIndex := strings.Index(entrypoint, marker)
		if markerIndex < 0 {
			t.Errorf("startup step %q is gone; check that the drop still precedes what replaced it", marker)
			continue
		}
		if markerIndex < dropIndex[1] {
			t.Errorf("%q runs before forwarding is dropped, leaving the sandbox unfiltered", marker)
		}
	}
	if rulesetIndex < dropIndex[1] {
		t.Error("the generated ruleset is applied before the default-drop table exists")
	}
}

// TestGatewayEntrypointResolvesHostGatewayFromHostsFile pins the host gateway lookup against the
// regression it fixes: `getent hosts host.internal` answers AF_INET6 first, so on a runtime that
// injects both an IPv4 and an IPv6 entry (OrbStack) an IPv4-only filter got nothing and the feature
// silently pointed every configured name at the sandbox container itself.
func TestGatewayEntrypointResolvesHostGatewayFromHostsFile(t *testing.T) {
	data, err := FS.ReadFile("gateway/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(data)
	libData, err := FS.ReadFile("gateway/hole-gateway-lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The lookup lives in the library the entrypoint and the apply script share.
	lib := string(libData)
	if !strings.Contains(entrypoint, "source /usr/local/bin/hole-gateway-lib.sh") {
		t.Fatal("the entrypoint no longer sources the shared gateway library")
	}

	if strings.Contains(entrypoint+lib, "getent hosts host.internal") {
		t.Error("the host gateway address is looked up through getent again, which answers IPv6 first")
	}
	if !strings.Contains(lib, "/etc/hosts") {
		t.Fatal("the host gateway address is no longer read from /etc/hosts")
	}
	// Both names the runtimes use, or Podman's gateway would resolve to nothing.
	for _, name := range []string{"host.internal", "host.containers.internal"} {
		if !strings.Contains(lib, name) {
			t.Errorf("the lookup does not consider %q", name)
		}
	}
	if !strings.Contains(lib, `$1 !~ /:/`) {
		t.Error("the lookup no longer rejects IPv6 addresses, which cannot be substituted into the ruleset")
	}

	// The hard failure is what turns a silently broken feature into a message the user sees, and
	// it must fire only when a zone actually references the address.
	guard := regexp.MustCompile(`(?s)grep -q '\{HOST_GATEWAY_IP\}'.*?exit 1`)
	if !guard.MatchString(entrypoint) {
		t.Error("an unusable host gateway address no longer aborts startup, or the abort is not gated " +
			"on the generated Corefile carrying the placeholder")
	}
	// Empty, loopback and the unspecified address are the three answers that mean "no host
	// gateway"; substituting any of them points the configured names at the sandbox itself.
	if !strings.Contains(lib, `""|127.*|0.0.0.0)`) {
		t.Error("the set of unusable host gateway addresses changed; empty, loopback and 0.0.0.0 must all abort")
	}
}

func TestSchemaIsReadable(t *testing.T) {
	if len(Schema()) == 0 {
		t.Error("embedded schema is empty")
	}
}

// Update-only files are not part of the image, so changing one must not change the image tag.
func TestBuildInputsHashIgnoresUpdateOnlyFiles(t *testing.T) {
	base := fstest.MapFS{
		"agents/Dockerfile":             {Data: []byte("FROM x")},
		"agents/Dockerfile.update":      {Data: []byte("FROM y")},
		"agents/claude/install-user.sh": {Data: []byte("install")},
		"agents/claude/update-user.sh":  {Data: []byte("update")},
		"agents/claude/update-root.sh":  {Data: []byte("update")},
		"agents/claude/version.json":    {Data: []byte("{}")},
		"gateway/version.json":          {Data: []byte("{}")},
	}
	reference := buildInputsHash(base)
	changed := func(path string) fstest.MapFS {
		fsys := fstest.MapFS{}
		for name, file := range base {
			fsys[name] = file
		}
		fsys[path] = &fstest.MapFile{Data: []byte("changed")}
		return fsys
	}
	for _, path := range []string{"agents/Dockerfile.update", "agents/claude/update-user.sh",
		"agents/claude/update-root.sh", "agents/claude/version.json"} {
		if buildInputsHash(changed(path)) != reference {
			t.Errorf("changing %s changed the build inputs hash", path)
		}
	}
	for _, path := range []string{"agents/Dockerfile", "agents/claude/install-user.sh", "gateway/version.json"} {
		if buildInputsHash(changed(path)) == reference {
			t.Errorf("changing %s did not change the build inputs hash", path)
		}
	}
}

// TestGatewayApplyOrder pins the fail-closed order of a runtime apply: the transitional ruleset
// (old and new entries) goes in before dnsmasq and CoreDNS learn the new names, and the final
// ruleset only after CoreDNS stopped resolving removed names.
func TestGatewayApplyOrder(t *testing.T) {
	data, err := FS.ReadFile("gateway/hole-gateway-apply")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	body := script[strings.Index(script, "apply_config() {"):]
	steps := []string{`nft -f "${open_rules}"`, "restart_dnsmasq", "reload_coredns", `nft -f "${final_rules}"`,
		"delete_unused_sets"}
	previous := -1
	for _, step := range steps {
		index := strings.Index(body, step)
		if index < 0 {
			t.Fatalf("apply step %q is gone", step)
		}
		if index < previous {
			t.Errorf("apply step %q runs out of order", step)
		}
		previous = index
	}
	// Validation precedes every change, and persistence follows a successful apply only.
	validate := strings.Index(script, `nft -c -f "${staging}/final.nft.resolved"`)
	firstApply := strings.Index(script, `if ! apply_config`)
	persist := strings.Index(script, `mv -T "${HOLE_STATE_DIR}/runtime.tmp"`)
	if validate < 0 || firstApply < 0 || persist < 0 || validate > firstApply || firstApply > persist {
		t.Error("the apply script no longer validates first and persists last")
	}
	for _, forbidden := range []string{"flush set", "destroy set"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the apply script uses %q, which fails on a missing set or an older kernel", forbidden)
		}
	}
}

// Image builds must not call the GitHub REST API (issue #66): its anonymous quota of 60 requests
// per hour per IP is shared behind a NAT or VPN exit, and an exhausted one failed the build with 403.
func TestNoAssetCallsTheGitHubAPI(t *testing.T) {
	err := fs.WalkDir(FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(FS, path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), "api.github.com") {
			t.Errorf("%s calls api.github.com; resolve releases from the github.com/<repo>/releases/latest redirect instead", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
