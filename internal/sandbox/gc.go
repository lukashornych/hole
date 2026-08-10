package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

const (
	// networkGracePeriod protects a concurrent start that has created its networks but not
	// yet started any container.
	networkGracePeriod = "10m"
	// tmpDirMaxAge is when an abandoned run directory becomes collectable. A long-lived
	// legitimate session is protected by its instance still being registered.
	tmpDirMaxAge = 24 * time.Hour
	// resourcePrefix is the name every Hole sandbox resource starts with.
	resourcePrefix = "hole-sandbox-"
	// dindVolumePrefix is the name prefix of per-instance Docker-in-Docker volumes.
	dindVolumePrefix = "hole-sandbox-docker-data-"
)

// GC removes leftovers from sandboxes that are no longer running. It is best-effort: every
// removal is logged, no failure aborts the caller, and anything that might belong to a
// concurrent start is left alone.
//
// It runs on every `start` and `list`, which is the backstop for the one case the watchdog
// cannot cover: both the CLI and the watchdog killed at once.
func GC(containerEngine *engine.Engine, host hostenv.Host, store *state.Store) {
	collectAbandonedInstances(containerEngine, host, store)
	collectNetworks(containerEngine)
	collectContainers(containerEngine, store)
	collectVolumes(containerEngine, store)
	collectTmpDirs(host, store)
}

// collectAbandonedInstances tears down instances whose CLI *and* watchdog are both gone.
// The state file is what makes this distinguishable from a healthy concurrent sandbox, so
// running containers may be removed here.
func collectAbandonedInstances(containerEngine *engine.Engine, host hostenv.Host, store *state.Store) {
	instances, err := store.List()
	if err != nil {
		logging.Debug("could not read the instance registry: %v", err)
		return
	}
	for _, instance := range instances {
		if !store.Abandoned(instance) {
			continue
		}
		logging.Info("Cleaning up abandoned sandbox %s (started %s ago)",
			instance.InstanceName, instance.Uptime().Round(time.Second))
		Teardown(containerEngine, host, store, instance)
	}
}

// collectNetworks prunes Hole's unattached networks. The age filter is what protects a
// concurrent start whose network exists but whose containers have not been created yet.
func collectNetworks(containerEngine *engine.Engine) {
	if err := containerEngine.NetworkPrune(engine.LabelManaged+"=true", networkGracePeriod); err != nil {
		logging.Debug("network prune failed: %v", err)
	}
}

// collectContainers removes stopped sandbox containers whose instance has no running
// container and no network left. The network check matters: compose has a window in which it
// has created its networks but not yet started its first container, and without it GC would
// reap a concurrent start out from under it.
func collectContainers(containerEngine *engine.Engine, store *state.Store) {
	containers := containerEngine.Containers(resourcePrefix)
	if len(containers) == 0 {
		return
	}

	byInstance := map[string][]engine.ContainerInfo{}
	for _, container := range containers {
		byInstance[instanceOfContainer(container.Name)] = append(byInstance[instanceOfContainer(container.Name)], container)
	}

	for instanceName, group := range byInstance {
		if instanceName == "" || store.Exists(instanceName) {
			continue
		}
		if anyRunning(group) {
			continue
		}
		if instanceHasNetworks(containerEngine, instanceName) {
			continue
		}
		ids := make([]string, 0, len(group))
		for _, container := range group {
			ids = append(ids, container.ID)
		}
		logging.Info("Removing %d stopped container(s) of %s", len(ids), instanceName)
		if err := containerEngine.ContainerRemove(ids...); err != nil {
			logging.Warn("could not remove containers of %s: %v", instanceName, err)
		}
	}
}

// collectVolumes removes per-instance Docker-in-Docker volumes whose instance has neither a
// network nor a container left. Volume prune has no age filter, so the liveness of sibling
// resources is the age proxy — which is why instance volumes are created *after* the
// networks during startup.
func collectVolumes(containerEngine *engine.Engine, store *state.Store) {
	for _, volume := range containerEngine.VolumesByName(dindVolumePrefix) {
		instanceName := strings.TrimPrefix(volume, dindVolumePrefix)
		if instanceName == volume || store.Exists(instanceName) {
			continue
		}
		if instanceHasNetworks(containerEngine, instanceName) || len(containerEngine.Containers(instanceName)) > 0 {
			continue
		}
		logging.Info("Removing orphaned Docker-in-Docker volume %s", volume)
		if err := containerEngine.VolumeRemove(volume); err != nil {
			logging.Warn("could not remove volume %s: %v", volume, err)
		}
	}
}

// collectTmpDirs removes run directories that no registered instance owns and that are old
// enough not to belong to a start currently in progress.
func collectTmpDirs(host hostenv.Host, store *state.Store) {
	entries, err := os.ReadDir(host.TmpRoot())
	if err != nil {
		return
	}
	live := map[string]bool{}
	if instances, err := store.List(); err == nil {
		for _, instance := range instances {
			live[instance.RunTmpDir] = true
		}
	}
	cutoff := time.Now().Add(-tmpDirMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run.") {
			continue
		}
		path := filepath.Join(host.TmpRoot(), entry.Name())
		if live[path] {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		logging.Debug("removing stale run directory %s", path)
		if err := os.RemoveAll(path); err != nil {
			logging.Warn("could not remove stale run directory %s: %v", path, err)
		}
	}
}

// instanceOfContainer strips compose's `-<service>-<replica>` suffix from a container name.
// It returns "" for anything that is not shaped like a Hole sandbox container, because a
// mis-parsed name would point GC at resources that are not ours.
func instanceOfContainer(name string) string {
	if !strings.HasPrefix(name, resourcePrefix) {
		return ""
	}
	parts := strings.Split(name, "-")
	// hole | sandbox | <project…> | <id> | <service> | <replica>
	if len(parts) < 6 {
		return ""
	}
	if _, err := strconv.Atoi(parts[len(parts)-1]); err != nil {
		return ""
	}
	instanceName := strings.Join(parts[:len(parts)-2], "-")
	if instanceName == strings.TrimSuffix(resourcePrefix, "-") {
		return ""
	}
	return instanceName
}

func anyRunning(containers []engine.ContainerInfo) bool {
	for _, container := range containers {
		if container.Running() {
			return true
		}
	}
	return false
}

func instanceHasNetworks(containerEngine *engine.Engine, instanceName string) bool {
	return len(containerEngine.NetworkNames(instanceName+"_")) > 0
}
