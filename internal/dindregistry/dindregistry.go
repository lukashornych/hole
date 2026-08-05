// Package dindregistry manages the pull-through image cache that Docker-in-Docker sandboxes
// use instead of a shared /var/lib/docker volume.
//
// The 1.x design seeded a per-instance volume from a shared cache and synced it back under a
// flock on exit — a serialisation point that raced and occasionally wiped the cache. A
// long-running mirror replaces all of it: each sandbox still gets a throwaway volume, but a
// second pull of the same image comes from the mirror instead of the internet.
package dindregistry

import (
	"fmt"
	"net/netip"

	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/logging"
)

const (
	// ContainerName is the long-lived mirror container.
	ContainerName = "hole-registry"
	// VolumeName holds the mirror's cached blobs.
	VolumeName = "hole-registry-data"
	// NetworkName is the mirror's own bridge network, which has normal egress: the mirror only
	// ever talks to Docker Hub, and it is host-side infrastructure rather than part of any
	// sandbox's trust boundary.
	NetworkName = "hole-registry-net"
	// Image is the upstream registry implementation.
	Image = "registry:2"
	// Port is the registry port inside the sandbox network.
	Port = "5000"
	// MirrorURL is what the DinD daemon is pointed at.
	MirrorURL = "http://" + ContainerName + ":" + Port

	// upstream is the registry the mirror proxies.
	upstream = "https://registry-1.docker.io"
	// networkSubnet keeps the mirror's own network out of Hole's sandbox pool.
	networkSubnet = "10.223.254.0/24"
)

// Ensure starts the mirror if it is not already running, and returns whether it is usable.
//
// Every failure is non-fatal: without the mirror DinD still works, it just pulls from the
// internet each time. Breaking a sandbox over a cache would be the wrong trade.
func Ensure(containerEngine *engine.Engine) bool {
	if err := ensureNetwork(containerEngine); err != nil {
		logging.Warn("could not prepare the image cache network, Docker-in-Docker will pull without a cache: %v", err)
		return false
	}

	for _, container := range containerEngine.Containers(ContainerName) {
		if container.Name != ContainerName {
			continue
		}
		if container.Running() {
			return true
		}
		// A stopped mirror is restarted rather than recreated: its volume holds the cache.
		if err := containerEngine.RunQuiet("start", ContainerName); err != nil {
			logging.Debug("could not restart %s, recreating it: %v", ContainerName, err)
			_ = containerEngine.ContainerRemove(ContainerName)
			break
		}
		return true
	}

	if !containerEngine.VolumeExists(VolumeName) {
		if err := containerEngine.VolumeCreate(VolumeName, map[string]string{engine.LabelManaged: "true"}); err != nil {
			logging.Warn("could not create the image cache volume: %v", err)
			return false
		}
	}

	logging.Info("Starting the Docker image cache (%s)...", ContainerName)
	err := containerEngine.RunQuiet("run", "-d",
		"--name", ContainerName,
		"--network", NetworkName,
		"--restart", "unless-stopped",
		"--label", engine.LabelManaged+"=true",
		"-v", VolumeName+":/var/lib/registry",
		"-e", "REGISTRY_PROXY_REMOTEURL="+upstream,
		Image)
	if err != nil {
		logging.Warn("could not start the image cache, Docker-in-Docker will pull without one: %v", err)
		return false
	}
	return true
}

// Attach connects the mirror to a sandbox network so the DinD daemon can reach it without
// having internet access of its own.
func Attach(containerEngine *engine.Engine, sandboxNetwork string) bool {
	if err := containerEngine.NetworkConnect(sandboxNetwork, ContainerName); err != nil {
		logging.Warn("could not attach the image cache to %s: %v", sandboxNetwork, err)
		return false
	}
	return true
}

// Detach disconnects the mirror from a sandbox network. Best-effort: teardown never aborts,
// and a leftover connection disappears with the network anyway.
func Detach(containerEngine *engine.Engine, sandboxNetwork string) {
	if err := containerEngine.NetworkDisconnect(sandboxNetwork, ContainerName); err != nil {
		logging.Debug("could not detach the image cache from %s: %v", sandboxNetwork, err)
	}
}

// Remove deletes the mirror and its cache. It is only a cache, so `hole destroy` (with no
// path) and uninstall may take it.
func Remove(containerEngine *engine.Engine) {
	if len(containerEngine.Containers(ContainerName)) > 0 {
		logging.Info("Removing the Docker image cache...")
		if err := containerEngine.ContainerRemove(ContainerName); err != nil {
			logging.Warn("could not remove %s: %v", ContainerName, err)
		}
	}
	if containerEngine.VolumeExists(VolumeName) {
		if err := containerEngine.VolumeRemove(VolumeName); err != nil {
			logging.Warn("could not remove %s: %v", VolumeName, err)
		}
	}
	if containerEngine.NetworkExists(NetworkName) {
		if err := containerEngine.NetworkRemove(NetworkName); err != nil {
			logging.Debug("could not remove %s: %v", NetworkName, err)
		}
	}
}

func ensureNetwork(containerEngine *engine.Engine) error {
	if containerEngine.NetworkExists(NetworkName) {
		return nil
	}
	subnet, err := netip.ParsePrefix(networkSubnet)
	if err != nil {
		return fmt.Errorf("parse cache network subnet: %w", err)
	}
	return containerEngine.NetworkCreate(engine.NetworkOptions{
		Name:   NetworkName,
		Subnet: subnet,
		Labels: map[string]string{engine.LabelManaged: "true"},
	})
}
