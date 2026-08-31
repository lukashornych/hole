package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/version"
)

// stateFile records what Hole knows about its own installation between runs.
const stateFile = "state.json"

// legacyInstallDir is where the 1.x tarball installer unpacked the bash implementation.
//
// The wrapper it also created lives at ~/.local/bin/hole — which is exactly where the binary
// now lives, so that path must never be part of this cleanup.
const legacyInstallDir = ".local/share/hole"

// legacyDockerCache is the shared /var/lib/docker cache volume the 1.x DinD support seeded
// per-instance volumes from. The pull-through registry replaced it.
const legacyDockerCache = "hole-sandbox-docker-cache"

// State is Hole's own persisted state.
type State struct {
	// LastVersion is the version that last completed a run, used to fire migrations once.
	LastVersion string `json:"lastVersion"`
}

// statePath is where State lives.
func statePath(host hostenv.Host) string { return filepath.Join(host.HoleDir(), stateFile) }

// LoadState reads Hole's own state. A missing or unreadable file is an empty state: this
// drives cleanup, so failing a run over it would be the wrong trade.
func LoadState(host hostenv.Host) State {
	data, err := os.ReadFile(statePath(host))
	if err != nil {
		return State{}
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}
	}
	return state
}

// SaveState persists Hole's own state.
func SaveState(host hostenv.Host, state State) error {
	if err := os.MkdirAll(host.HoleDir(), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", host.HoleDir(), err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hole state: %w", err)
	}
	if err := os.WriteFile(statePath(host), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", statePath(host), err)
	}
	return nil
}

// OnVersionChange runs the migrations for a new version exactly once, then records it.
//
// Everything here is best-effort: a leftover from an old version is untidy, not dangerous, and
// no cleanup failure may stop a sandbox from starting.
func OnVersionChange(host hostenv.Host, containerEngine *engine.Engine) {
	if !version.CanMigrate() {
		return
	}
	state := LoadState(host)
	if state.LastVersion == version.Version {
		return
	}

	if state.LastVersion == "" {
		logging.Info("First run of hole %s — cleaning up resources from the previous installation...", version.Version)
	} else {
		logging.Info("Updated from hole %s to %s — cleaning up superseded resources...", state.LastVersion, version.Version)
	}

	RemoveLegacyResources(containerEngine)
	RemoveLegacyInstall(host)

	state.LastVersion = version.Version
	if err := SaveState(host, state); err != nil {
		logging.Debug("could not record the current version: %v", err)
	}
}

// RemoveLegacyResources deletes Docker resources no version of Hole uses any more.
func RemoveLegacyResources(containerEngine *engine.Engine) {
	// The proxy and dns services no longer exist, and per-project `:latest` agent images were
	// replaced by hash-tagged ones.
	for _, reference := range []string{"hole-sandbox/proxy-*", "hole-sandbox/dns-*"} {
		for _, image := range containerEngine.ImagesByReference(reference) {
			removeLegacy(containerEngine.ImageRemove(image), "image", image)
		}
	}
	for _, image := range containerEngine.ImagesByReference("hole-sandbox/agent-*:latest") {
		removeLegacy(containerEngine.ImageRemove(image), "image", image)
	}

	// The shared DinD cache volume and the 1.x agent-home volumes.
	if containerEngine.VolumeExists(legacyDockerCache) {
		removeLegacy(containerEngine.VolumeRemove(legacyDockerCache), "volume", legacyDockerCache)
	}
	for _, volume := range containerEngine.VolumesByName("hole-sandbox-agent-home") {
		removeLegacy(containerEngine.VolumeRemove(volume), "volume", volume)
	}

	// Networks from 1.x carry no labels, which is what distinguishes them from a network a
	// concurrent start of *this* version has just created and not yet attached anything to.
	for _, info := range legacyNetworks(containerEngine) {
		removeLegacy(containerEngine.NetworkRemove(info), "network", info)
	}
}

// legacyNetworks lists Hole-named networks without Hole's labels.
func legacyNetworks(containerEngine *engine.Engine) []string {
	infos, err := containerEngine.Networks()
	if err != nil {
		return nil
	}
	var names []string
	for _, info := range infos {
		if !strings.HasPrefix(info.Name, "hole-sandbox-") {
			continue
		}
		if info.Labels[engine.LabelManaged] == "true" {
			continue
		}
		if info.Containers > 0 {
			continue
		}
		names = append(names, info.Name)
	}
	return names
}

// RemoveLegacyInstall deletes the 1.x tarball installation directory.
func RemoveLegacyInstall(host hostenv.Host) {
	directory := filepath.Join(host.Home, legacyInstallDir)
	if _, err := os.Stat(directory); err != nil {
		return
	}
	if err := os.RemoveAll(directory); err != nil {
		logging.Warn("could not remove the previous installation at %s: %v", directory, err)
		return
	}
	logging.Info("Removed the previous bash installation at %s", directory)
}

func removeLegacy(err error, kind, name string) {
	if err != nil {
		logging.Debug("could not remove superseded %s %s: %v", kind, name, err)
		return
	}
	logging.Info("Removed superseded %s %s", kind, name)
}
