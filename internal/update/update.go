package update

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/version"
)

// installOneLiner is the fallback when self-update cannot work — a read-only install
// directory, or a binary installed by a package manager.
const installOneLiner = "curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash"

// SelfUpdate replaces the running binary with the newest release.
func SelfUpdate() error {
	if !version.CanSelfUpdate() {
		return fmt.Errorf("cannot update this build; %s", upgradeHint(version.BuildKind(), version.Version, ""))
	}

	logging.Info("Checking for updates...")
	release, err := FetchLatest(30 * time.Second)
	if err != nil {
		return err
	}
	if !version.GreaterThan(release.Version(), version.Version) {
		logging.Info("hole is already up to date (version %s).", version.Version)
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	executable = resolveInstallPath(executable)

	assetName := BinaryAssetName()
	binaryURL, ok := release.AssetURL(assetName)
	if !ok {
		return fmt.Errorf("release %s has no %s build; install manually with:\n  %s",
			release.TagName, assetName, installOneLiner)
	}
	checksumsURL, ok := release.AssetURL(checksumsAsset)
	if !ok {
		return fmt.Errorf("release %s publishes no %s, refusing to install an unverified binary",
			release.TagName, checksumsAsset)
	}

	logging.Info("Updating hole: %s -> %s", version.Version, release.Version())

	checksums, err := download(checksumsURL)
	if err != nil {
		return err
	}
	expected, err := ExpectedChecksum(checksums, assetName)
	if err != nil {
		return err
	}
	payload, err := download(binaryURL)
	if err != nil {
		return err
	}
	if err := verifyChecksum(payload, expected); err != nil {
		return fmt.Errorf("refusing to install %s: %w", assetName, err)
	}

	if err := replaceBinary(executable, payload); err != nil {
		return fmt.Errorf("%w\ninstall manually with:\n  %s", err, installOneLiner)
	}
	logging.Info("hole updated to %s.", release.Version())
	return nil
}

// resolveInstallPath follows symlinks so the real file is replaced rather than the link.
//
// EvalSymlinks rather than Readlink: a link target is often relative
// (`../Cellar/hole/2.0.0/bin/hole` for a Homebrew-style install), and replaceBinary would then
// write and rename relative to the process working directory — leaving the installed binary
// untouched while reporting success. An unresolvable path keeps the original, which fails
// visibly in replaceBinary instead of aborting an otherwise working update here.
func resolveInstallPath(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil && resolved != "" {
		return resolved
	}
	return executable
}

// CheckForUpdate prints a notice when a newer release exists. It is silent on any failure and
// gives up after a second — it must never delay a sandbox start.
//
// The GitHub release is the single source for every build kind: it is always reachable, and unlike
// the module proxy it also sees a release that crossed into a new major version.
func CheckForUpdate() {
	if !version.CanCompare() {
		return
	}
	release, err := FetchLatest(time.Second)
	if err != nil {
		return
	}
	latest := release.Version()
	if !version.GreaterThan(latest, version.Version) {
		return
	}
	if version.CanSelfUpdate() {
		logging.Info("A new version of hole is available: %s (installed: %s). Run 'hole update' to upgrade.",
			latest, version.Version)
		return
	}
	logging.Info("A new version of hole is available: %s (installed: %s) — %s",
		latest, version.Version, upgradeHint(version.BuildKind(), version.Version, latest))
}

// upgradeHint names the way this particular build is upgraded. Self-update serves release binaries
// only, so a `go install` build gets the command that actually upgrades it — including the changed
// module path when the newest release crossed a major version, which `@latest` on the installed
// path would never reach on its own. An empty latest means the newest release is not known, which
// is the case when the caller has not asked GitHub.
func upgradeHint(kind version.Kind, installed, latest string) string {
	if kind != version.Source {
		return "build from source or install a release with:\n  " + installOneLiner
	}
	major := version.Major(installed)
	if latest != "" {
		major = version.Major(latest)
	}
	command := version.GoInstallCommand(major)
	if command == "" {
		return "upgrade by re-running the go install command you used"
	}
	if major > version.Major(installed) {
		return fmt.Sprintf("that is a new major version, so the module path changes too — upgrade with:\n  %s", command)
	}
	return "upgrade with:\n  " + command
}
