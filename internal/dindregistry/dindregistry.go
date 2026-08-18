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
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/logging"
)

const (
	// ContainerName is the long-lived mirror container.
	ContainerName = "hole-registry"
	// VolumeName holds the mirror's cached blobs.
	VolumeName = "hole-registry-data"
	// NetworkName is the mirror's own bridge network, which has normal egress: the mirror only
	// ever talks to Docker Hub. It is host-side infrastructure, but while Attach has it connected
	// to a sandbox network it is also a dual-homed container inside that sandbox's trust
	// boundary — which is why the caller attaches it only to sandboxes that allowed Docker Hub.
	NetworkName = "hole-registry-net"
	// Image is the upstream registry implementation, pinned by digest so the mirror's content
	// cannot change under a fixed Hole version. This is the `registry:2` multi-arch index as
	// published on 2026-08-06; the tag is kept in this comment rather than in the reference,
	// since the digest alone identifies the image and a tag beside it can drift out of sync.
	// A bump only reaches an existing mirror once its container is removed — Ensure restarts a
	// stopped `hole-registry` rather than recreating it, to keep the cache volume.
	Image = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	// Port is the registry port inside the sandbox network.
	Port = "5000"
	// MirrorURL is what the DinD daemon is pointed at.
	MirrorURL = "http://" + ContainerName + ":" + Port

	// upstream is the registry the mirror proxies.
	upstream = "https://registry-1.docker.io"
	// networkSubnet keeps the mirror's own network out of Hole's sandbox pool.
	networkSubnet = "10.223.254.0/24"
	// restartPolicy is capped rather than `unless-stopped`: the registry exits during startup when
	// its upstream is unreachable, and an uncapped policy turned that into a container restarting
	// forever, outliving every sandbox that asked for it.
	restartPolicy = "on-failure:5"
)

const (
	// readyStable is how long the mirror has to stay up, without the runtime restarting it, before
	// it counts as usable. The registry aborts during startup when it cannot reach its upstream,
	// so staying up is the signal — a container that is merely `created` proves nothing.
	readyStable = 2 * time.Second
	// readyTimeout bounds the probe, and readyPoll is how often it samples the container's state.
	readyTimeout = 15 * time.Second
	readyPoll    = 250 * time.Millisecond
)

// Ensure starts the mirror if it is not already running, and returns whether it is usable.
//
// Every failure is non-fatal: without the mirror DinD still works, it just pulls from the
// internet each time. Breaking a sandbox over a cache would be the wrong trade.
func Ensure(containerEngine *engine.Engine) bool {
	if err := ensureNetwork(containerEngine); err != nil {
		logging.Warn("could not prepare the image cache network, so Docker-in-Docker has no cache — "+
			"Docker Hub pulls will fail unless the allow list covers Hub's endpoints directly: %v", err)
		return false
	}

	for _, container := range containerEngine.Containers(ContainerName) {
		if container.Name != ContainerName {
			continue
		}
		// A stopped mirror is restarted rather than recreated: its volume holds the cache.
		if !container.Running() {
			if err := containerEngine.RunQuiet("start", ContainerName); err != nil {
				logging.Debug("could not restart %s, recreating it: %v", ContainerName, err)
				discard(containerEngine)
				break
			}
		}
		if !waitUntilServing(containerEngine, ContainerName) {
			reportUnusable(containerEngine, ContainerName)
			discard(containerEngine)
			return false
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
		"--restart", restartPolicy,
		"--label", engine.LabelManaged+"=true",
		"-v", VolumeName+":/var/lib/registry",
		"-e", "REGISTRY_PROXY_REMOTEURL="+upstream,
		Image)
	if err != nil {
		logging.Warn("could not start the image cache, so Docker-in-Docker has none — Docker Hub pulls "+
			"will fail unless the allow list covers Hub's endpoints directly: %v", err)
		return false
	}
	if !waitUntilServing(containerEngine, ContainerName) {
		reportUnusable(containerEngine, ContainerName)
		discard(containerEngine)
		return false
	}
	return true
}

// waitUntilServing reports whether a mirror container is usable.
//
// `docker run -d` succeeding says nothing about the registry: with its upstream unreachable it
// aborts during startup, and accepting that container pointed the DinD daemon at a dead endpoint.
// The signal is therefore that the container stays up and the runtime does not restart it — state
// only, never a log string, so an upstream change of wording cannot fail a healthy mirror. It
// catches a mirror that never came up; one that dies later still leaves the daemon a stale
// `--registry-mirror`, which dockerd falls back from to the upstream registry.
func waitUntilServing(containerEngine *engine.Engine, container string) bool {
	// Restarts are counted from here, not from zero: a mirror that crashed once days ago and has
	// been serving since is usable, and discarding it would throw away the cache.
	restartsAtEntry, _ := containerEngine.ContainerRestartCount(container)
	deadline := time.Now().Add(readyTimeout)
	upSince := time.Now()
	for {
		if !containerEngine.ContainerRunning(container) {
			return false
		}
		if restarts, ok := containerEngine.ContainerRestartCount(container); ok && restarts > restartsAtEntry {
			return false
		}
		if time.Since(upSince) >= readyStable {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(readyPoll)
	}
}

// reportUnusable names why the mirror was refused. The registry's own last line carries the cause
// — an unreachable upstream, most often — which a warning about a missing cache would not.
func reportUnusable(containerEngine *engine.Engine, container string) {
	detail := ""
	if code, ok := containerEngine.ContainerExitCode(container); ok && code != 0 {
		detail = fmt.Sprintf(" (exit code %d)", code)
	}
	if logs, err := containerEngine.ContainerLogs(container); err == nil {
		lines := strings.Split(strings.TrimSpace(logs), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			detail += ": " + last
		}
	}
	logging.Warn("the Docker image cache did not come up%s, so Docker-in-Docker has none — Docker Hub "+
		"pulls will go to the internet each time", detail)
}

// discard removes a mirror that cannot serve, so the next start makes a clean attempt instead of
// finding a crash-looping container. The cache volume is kept: it is the part worth keeping, and
// a fresh container re-uses it. A running container is never taken — a concurrent sandbox may be
// pulling through it.
func discard(containerEngine *engine.Engine) {
	if containerEngine.ContainerRunning(ContainerName) {
		return
	}
	if err := containerEngine.ContainerRemove(ContainerName); err != nil {
		logging.Debug("could not remove the unusable image cache: %v", err)
	}
}

// Attach connects the mirror to a sandbox network so the DinD daemon can reach it without
// having internet access of its own.
//
// Sandbox-internal traffic is unfiltered, so this bypasses the gateway's allow-list: callers must
// only attach sandboxes whose policy allows Docker Hub (`network.Policy.AllowsDockerHub`).
func Attach(containerEngine *engine.Engine, sandboxNetwork string) bool {
	if err := containerEngine.NetworkConnect(sandboxNetwork, ContainerName); err != nil {
		logging.Warn("could not attach the image cache to %s, so Docker Hub pulls will fail unless the "+
			"allow list covers Hub's endpoints directly: %v", sandboxNetwork, err)
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

// Stop shuts the mirror down while keeping the container and its cache volume, so `Ensure`
// brings the same cache back on the next start instead of re-downloading into a new one.
//
// The caller decides when nothing needs the mirror any more — teardown does, once the sandbox
// leaving is the last one. Best-effort like the rest of teardown: a mirror that stays up costs
// idle memory, nothing more.
func Stop(containerEngine *engine.Engine) {
	if !containerEngine.ContainerRunning(ContainerName) {
		return
	}
	logging.Info("Stopping the Docker image cache (no sandboxes left)...")
	if err := containerEngine.ContainerStop(ContainerName); err != nil {
		logging.Warn("could not stop %s: %v", ContainerName, err)
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
