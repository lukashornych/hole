// Package update handles Hole's own lifecycle: replacing the running binary with a newer
// release, cleaning up resources a previous version left behind, and uninstalling.
//
// Self-update is implemented on the standard library rather than a self-update package. The
// three things such a package provides — release discovery, checksum verification and atomic
// replacement — are a hundred lines here, and the alternative pulled in ~50 modules
// (CEL, antlr, protovalidate, the Gitea SDK, dbus, wincred), which is at odds with shipping
// one small static binary.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// latestReleaseURL is the GitHub API endpoint for the newest release.
	latestReleaseURL = "https://api.github.com/repos/lukashornych/hole/releases/latest"
	// checksumsAsset is the release asset listing the sha256 of every binary.
	checksumsAsset = "checksums.txt"
	// downloadTimeout bounds the whole download, which is a few megabytes.
	downloadTimeout = 5 * time.Minute
)

// Release is the subset of a GitHub release Hole needs.
type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Version is the release version without the leading `v`.
func (r Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// AssetURL returns the download URL of one asset.
func (r Release) AssetURL(name string) (string, bool) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset.URL, true
		}
	}
	return "", false
}

// BinaryAssetName is the release asset for the running platform. GoReleaser publishes raw
// binaries under this name, so neither the installer nor self-update has to unpack anything.
func BinaryAssetName() string {
	return fmt.Sprintf("hole_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// FetchLatest reads the newest release from the GitHub API.
func FetchLatest(timeout time.Duration) (Release, error) {
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(latestReleaseURL)
	if err != nil {
		return Release{}, fmt.Errorf("check for the latest release: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("check for the latest release: GitHub returned %s", response.Status)
	}
	var release Release
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return Release{}, fmt.Errorf("parse the latest release: %w", err)
	}
	if release.TagName == "" {
		return Release{}, fmt.Errorf("the latest release has no tag")
	}
	return release, nil
}

// download fetches a URL into memory.
func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: downloadTimeout}
	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: server returned %s", url, response.Status)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return data, nil
}

// ExpectedChecksum finds one file's sha256 in a GoReleaser checksums.txt.
func ExpectedChecksum(checksums []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// The name may be prefixed with `*` for binary mode.
		if strings.TrimPrefix(fields[1], "*") == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s does not list %s", checksumsAsset, assetName)
}

// verifyChecksum fails unless the payload matches the expected digest. An unverified binary
// is never written to disk: this is the one step that makes downloading an executable over
// the network defensible.
func verifyChecksum(payload []byte, expected string) error {
	sum := sha256.Sum256(payload)
	actual := hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// replaceBinary swaps the file at path for new content.
//
// The replacement is written next to the target and renamed over it, which is atomic within a
// filesystem: an interrupted update can never leave a half-written binary in place. Renaming
// over a *running* executable is fine on Unix — the running process keeps its own inode.
func replaceBinary(path string, payload []byte) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".hole-update-")
	if err != nil {
		return fmt.Errorf("create the replacement binary in %s: %w", directory, err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write the replacement binary: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write the replacement binary: %w", err)
	}

	mode := os.FileMode(0o755)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tempPath, mode); err != nil {
		return fmt.Errorf("set permissions on the replacement binary: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
