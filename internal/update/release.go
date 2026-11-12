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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// releasesURL is the web page of Hole's releases. Release discovery deliberately avoids
	// api.github.com: its anonymous quota of 60 requests per hour per IP is shared by everyone
	// behind the same NAT or VPN exit, and an exhausted quota answers 403 to `hole update`.
	releasesURL = "https://github.com/lukashornych/hole/releases"
	// checksumsAsset is the release asset listing the sha256 of every binary.
	checksumsAsset = "checksums.txt"
	// downloadTimeout bounds the whole download, which is a few megabytes.
	downloadTimeout = 5 * time.Minute
)

// errNotFound marks a download the server answered with 404 — for a release asset, one the
// release does not publish.
var errNotFound = errors.New("not found")

// Release is a published GitHub release, identified by its tag.
type Release struct {
	TagName string
}

// Version is the release version without the leading `v`.
func (r Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// AssetURL returns the download URL of one asset. The URL is built rather than listed, so whether
// the release publishes the asset shows only when the download answers 404.
func (r Release) AssetURL(name string) string {
	return releasesURL + "/download/" + r.TagName + "/" + name
}

// BinaryAssetName is the release asset for the running platform. GoReleaser publishes raw
// binaries under this name, so neither the installer nor self-update has to unpack anything.
func BinaryAssetName() string {
	return fmt.Sprintf("hole_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// FetchLatest finds the newest release.
func FetchLatest(timeout time.Duration) (Release, error) {
	return fetchLatest(releasesURL+"/latest", timeout)
}

// fetchLatest reads the tag from the redirect `<releases>/latest` answers with, which points at
// `<releases>/tag/<tag>`. The redirect is not followed: one round trip is all the one-second
// budget of CheckForUpdate affords, and the release page itself carries nothing more.
func fetchLatest(latestURL string, timeout time.Duration) (Release, error) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Head(latestURL)
	if err != nil {
		return Release{}, fmt.Errorf("check for the latest release: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	location, err := response.Location()
	if err != nil {
		return Release{}, fmt.Errorf("check for the latest release: GitHub returned %s instead of a redirect to it",
			response.Status)
	}
	directory, tag := path.Split(location.Path)
	if path.Base(directory) != "tag" || tag == "" {
		return Release{}, fmt.Errorf("check for the latest release: GitHub redirected to %s, which names no release tag",
			location)
	}
	return Release{TagName: tag}, nil
}

// download fetches a URL into memory.
func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: downloadTimeout}
	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("download %s: %w", url, errNotFound)
	}
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
