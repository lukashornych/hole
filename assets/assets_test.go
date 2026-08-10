package assets

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedTreeIsComplete(t *testing.T) {
	required := []string{
		"agents/Dockerfile",
		"agents/entrypoint.sh",
		"agents/claude/command.json",
		"agents/claude/allow.txt",
		"agents/gemini/command.json",
		"agents/gemini/allow.txt",
		"agents/codex/command.json",
		"agents/codex/allow.txt",
		"gateway/Dockerfile",
		"gateway/entrypoint.sh",
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

func TestSchemaIsReadable(t *testing.T) {
	if len(Schema()) == 0 {
		t.Error("embedded schema is empty")
	}
}
