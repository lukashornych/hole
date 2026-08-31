package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/version"
)

func TestBinaryAssetNameMatchesThePlatform(t *testing.T) {
	want := "hole_" + runtime.GOOS + "_" + runtime.GOARCH
	if got := BinaryAssetName(); got != want {
		t.Errorf("BinaryAssetName() = %q, want %q", got, want)
	}
	// The installer greps checksums.txt for this exact name, so the two must stay in step.
	installer, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Skipf("install.sh not readable: %v", err)
	}
	if !strings.Contains(string(installer), `asset="hole_${platform}"`) {
		t.Error("install.sh no longer builds the asset name as hole_<os>_<arch>")
	}
}

func TestReleaseAccessors(t *testing.T) {
	release := Release{TagName: "v2.1.0"}
	release.Assets = append(release.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{Name: "hole_linux_arm64", URL: "https://example.com/hole_linux_arm64"})

	if release.Version() != "2.1.0" {
		t.Errorf("Version() = %q, want 2.1.0", release.Version())
	}
	if url, ok := release.AssetURL("hole_linux_arm64"); !ok || url == "" {
		t.Errorf("AssetURL = %q, %v", url, ok)
	}
	if _, ok := release.AssetURL("hole_windows_amd64"); ok {
		t.Error("a missing asset was reported as present")
	}
}

func TestExpectedChecksum(t *testing.T) {
	checksums := []byte(
		"aaaa  hole_linux_amd64\n" +
			"bbbb *hole_linux_arm64\n" +
			"cccc  hole_darwin_arm64\n")

	for asset, want := range map[string]string{
		"hole_linux_amd64":  "aaaa",
		"hole_linux_arm64":  "bbbb",
		"hole_darwin_arm64": "cccc",
	} {
		got, err := ExpectedChecksum(checksums, asset)
		if err != nil {
			t.Fatalf("ExpectedChecksum(%s): %v", asset, err)
		}
		if got != want {
			t.Errorf("ExpectedChecksum(%s) = %q, want %q", asset, got, want)
		}
	}

	if _, err := ExpectedChecksum(checksums, "hole_darwin_amd64"); err == nil {
		t.Error("a missing asset must be an error, not an empty checksum")
	}
}

func TestVerifyChecksum(t *testing.T) {
	payload := []byte("binary content")
	sum := sha256.Sum256(payload)
	expected := hex.EncodeToString(sum[:])

	if err := verifyChecksum(payload, expected); err != nil {
		t.Errorf("a matching checksum was rejected: %v", err)
	}
	// A mismatch must fail: this is the only thing standing between a network download and
	// an executable on disk.
	if err := verifyChecksum([]byte("tampered"), expected); err == nil {
		t.Error("a mismatched checksum was accepted")
	}
}

// A relative link target used to be taken verbatim, so the update was written relative to the
// process working directory and the installed binary was never touched.
func TestResolveInstallPathFollowsARelativeSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "Cellar", "hole", "2.0.0", "bin")
	linkDir := filepath.Join(root, "bin")
	for _, dir := range []string{realDir, linkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realPath := filepath.Join(realDir, "hole")
	if err := os.WriteFile(realPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(linkDir, "hole")
	if err := os.Symlink("../Cellar/hole/2.0.0/bin/hole", linkPath); err != nil {
		t.Fatal(err)
	}

	// The temp root itself may be reached through a symlink (/var on macOS), so compare against
	// the resolved real path rather than the constructed one.
	want, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveInstallPath(linkPath); got != want {
		t.Errorf("resolveInstallPath = %q, want the absolute target %q", got, want)
	}

	// A path that cannot be resolved keeps the original, so the failure surfaces in
	// replaceBinary rather than aborting an otherwise working update.
	missing := filepath.Join(root, "does-not-exist")
	if got := resolveInstallPath(missing); got != missing {
		t.Errorf("resolveInstallPath(%q) = %q, want it unchanged", missing, got)
	}
}

func TestReplaceBinaryIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hole")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(path, []byte("new")); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Errorf("content = %q, want new", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — the replacement must stay executable", info.Mode().Perm())
	}
	// No temporary files may survive.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the binary to remain, got %d entries", len(entries))
	}
}

func TestSelfUpdateRefusesOnADevelopmentBuild(t *testing.T) {
	if version.BuildKind() != version.Development {
		t.Skip("test binary was built with a stamped version")
	}
	err := SelfUpdate()
	if err == nil {
		t.Fatal("a development build must refuse to self-update")
	}
	// The message has to point somewhere useful rather than just failing.
	if !strings.Contains(err.Error(), "install.sh") {
		t.Errorf("error should suggest the installer: %v", err)
	}
}

func TestUpgradeHintMatchesTheBuildKind(t *testing.T) {
	tests := []struct {
		name      string
		kind      version.Kind
		installed string
		latest    string
		want      string
	}{
		{
			name:      "a development build is pointed at the installer",
			kind:      version.Development,
			installed: version.DevelopmentVersion,
			want:      "build from source or install a release with:\n  " + installOneLiner,
		},
		{
			// The command itself comes from the path recorded in the binary — which is the test
			// binary here, so the expectation is built the same way; internal/version covers the
			// path rewriting itself.
			name:      "a go install build is told to re-run go install",
			kind:      version.Source,
			installed: "2.0.0",
			latest:    "2.1.0",
			want:      "upgrade with:\n  " + version.GoInstallCommand(2),
		},
		{
			// `@latest` on the installed path stops at the major boundary, so the hint has to
			// name the path that does not.
			name:      "a new major tells the go install user the path moves",
			kind:      version.Source,
			installed: "2.1.4",
			latest:    "3.0.0",
			want:      "that is a new major version, so the module path changes too — upgrade with:\n  " + version.GoInstallCommand(3),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := upgradeHint(test.kind, test.installed, test.latest)
			if got != test.want {
				t.Errorf("upgradeHint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStateRoundTrip(t *testing.T) {
	host := hostenv.Host{Home: t.TempDir()}

	if state := LoadState(host); state.LastVersion != "" {
		t.Errorf("a missing state file should read as empty, got %+v", state)
	}
	if err := SaveState(host, State{LastVersion: "2.0.0"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if state := LoadState(host); state.LastVersion != "2.0.0" {
		t.Errorf("LastVersion = %q, want 2.0.0", state.LastVersion)
	}
}

func TestLoadStateToleratesGarbage(t *testing.T) {
	host := hostenv.Host{Home: t.TempDir()}
	if err := os.MkdirAll(host.HoleDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(host), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupt state file drives cleanup, so it must degrade to "unknown version" rather
	// than failing a run.
	if state := LoadState(host); state.LastVersion != "" {
		t.Errorf("expected an empty state, got %+v", state)
	}
}

func TestRemoveLegacyInstall(t *testing.T) {
	host := hostenv.Host{Home: t.TempDir()}
	legacy := filepath.Join(host.Home, legacyInstallDir)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "hole.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The 1.x wrapper lived at ~/.local/bin/hole — which is where the binary lives now, so
	// the cleanup must never touch it.
	binary := filepath.Join(host.Home, ".local", "bin", "hole")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	RemoveLegacyInstall(host)

	if _, err := os.Stat(legacy); err == nil {
		t.Error("the legacy install directory survived")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Error("the cleanup removed the hole binary itself")
	}
}

func TestRemoveLegacyInstallWithNothingToDo(t *testing.T) {
	// Must be silent and safe on a clean machine.
	RemoveLegacyInstall(hostenv.Host{Home: t.TempDir()})
}

func TestConfirmRemoveSettings(t *testing.T) {
	tests := map[string]bool{"y\n": true, "Y\n": true, "yes\n": true, "n\n": false, "\n": false, "maybe\n": false}
	for answer, want := range tests {
		got := ConfirmRemoveSettings(strings.NewReader(answer), &strings.Builder{}, "/home/dev/.hole", true)
		if got != want {
			t.Errorf("answer %q = %v, want %v", strings.TrimSpace(answer), got, want)
		}
	}
	// Without a terminal nobody is there to answer, so user data stays.
	if ConfirmRemoveSettings(strings.NewReader("y\n"), &strings.Builder{}, "/home/dev/.hole", false) {
		t.Error("a non-interactive uninstall must keep the user directory")
	}
}

func TestConfirmRemoveSettingsPromptNamesTheDirectory(t *testing.T) {
	var out strings.Builder
	ConfirmRemoveSettings(strings.NewReader("n\n"), &out, "/home/dev/.hole", true)
	if !strings.Contains(out.String(), "/home/dev/.hole") {
		t.Errorf("prompt = %q, want it to name the directory", out.String())
	}
}
