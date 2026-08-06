package sandbox

import (
	"encoding/json"
	"os"
	"syscall"

	"github.com/lukashornych/hole/internal/config"
	"github.com/lukashornych/hole/internal/dindregistry"
	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hooks"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/state"
)

// Teardown removes every resource of one instance and deregisters it.
//
// This is the single teardown implementation: the watchdog runs it in the normal path, the
// CLI only if the watchdog died, and GC for abandoned instances. It is idempotent, never
// aborts, and names every leftover it could not remove.
//
// Nothing here is gated on how far startup got. That was the root cause behind the bash
// version's "cleanup seems random" symptom: `compose down` only ran when the final
// `up -d agent` had succeeded, so an earlier failure leaked containers, and the network
// removal then failed invisibly because containers were still attached.
func Teardown(containerEngine *engine.Engine, host hostenv.Host, store *state.Store, instance *state.Instance) {
	// The lock is a reentrancy guard, not a coordination mechanism: there is exactly one
	// intended executor at a time, and whoever loses the race finds the instance gone.
	unlock, locked := lockInstance(store.LockPath(instance.InstanceName))
	if locked {
		defer unlock()
		if !store.Exists(instance.InstanceName) {
			logging.Debug("instance %s was already torn down", instance.InstanceName)
			return
		}
	}

	// Re-read the registry: startup records each resource as it is created, so a supervisor
	// that read the file earlier holds a snapshot with no networks or volume in it — and
	// would silently leave exactly those behind.
	if fresh, err := store.Read(instance.InstanceName); err == nil {
		instance = fresh
	}

	// Whether the sandbox actually ran decides the closing message: an aborted startup
	// should not report a session that never happened. Started, not merely created — compose
	// creates the agent container while it waits for the gateway, so existence alone would
	// claim a session for a startup that failed before the agent ever ran.
	agentRan := containerEngine.ContainerStarted(instance.InstanceName + "-agent-1")

	if instance.Flags.DumpNetworkAccess && containerEngine.ContainerExists(instance.InstanceName+"-gateway-1") {
		writeNetworkAccessDump(containerEngine, host, instance)
	}

	if err := containerEngine.ComposeDown(instance.InstanceName); err != nil {
		logging.Debug("compose down failed, retrying: %v", err)
		if err := containerEngine.ComposeDown(instance.InstanceName); err != nil {
			logging.Warn("could not stop containers of %s: %v", instance.InstanceName, err)
		}
	}

	if instance.RegistryMirror != "" && len(instance.Networks) > 0 {
		dindregistry.Detach(containerEngine, instance.Networks[0])
	}

	removeNetworksOf(containerEngine, instance)

	if instance.DinDVolume != "" {
		if err := containerEngine.VolumeRemove(instance.DinDVolume); err != nil && containerEngine.VolumeExists(instance.DinDVolume) {
			logging.Warn("could not remove Docker-in-Docker volume %s", instance.DinDVolume)
		}
	}

	runCleanupHostHooks(host, instance)

	if instance.RunTmpDir != "" {
		if err := os.RemoveAll(instance.RunTmpDir); err != nil {
			logging.Warn("could not remove temporary directory %s: %v", instance.RunTmpDir, err)
		}
	}

	verifyRemoved(containerEngine, instance)

	if agentRan {
		logging.Line()
		logging.Info("Exited %s CLI. Sandbox destroyed.", instance.Agent)
	}

	// Deregistering comes last, after every line this teardown will ever print. The CLI mirrors
	// the watchdog's records by tailing the log and stops the moment the instance disappears
	// from the registry — so anything logged after this point races that check and is silently
	// dropped from the console, while still sitting in the log file.
	store.Remove(instance.InstanceName)
}

// removeNetworksOf removes the per-instance networks. Compose does not remove external
// networks, and a partial `down` can leave containers attached — so a failure is retried
// once after force-removing whatever is still on the network.
func removeNetworksOf(containerEngine *engine.Engine, instance *state.Instance) {
	for _, name := range instance.Networks {
		if !containerEngine.NetworkExists(name) {
			continue
		}
		if err := containerEngine.NetworkRemove(name); err == nil {
			continue
		}
		// The image cache is shared infrastructure, so it is disconnected rather than removed:
		// this path is reached exactly when the recorded Detach did not happen, and destroying
		// another sandbox's mirror to tear down this one would be the wrong trade.
		attached := withoutRegistryMirror(containerEngine.NetworkAttachedContainers(name))
		if len(attached) > 0 {
			logging.Debug("network %s still has %d attached containers, removing them", name, len(attached))
			_ = containerEngine.ContainerRemove(attached...)
		}
		dindregistry.Detach(containerEngine, name)
		if err := containerEngine.NetworkRemove(name); err != nil {
			logging.Warn("could not remove network %s — remove it manually with: docker network rm %s", name, name)
		}
	}
}

// withoutRegistryMirror filters the shared image cache out of a list of containers.
func withoutRegistryMirror(containers []string) []string {
	out := make([]string, 0, len(containers))
	for _, container := range containers {
		if container == dindregistry.ContainerName {
			continue
		}
		out = append(out, container)
	}
	return out
}

// runCleanupHostHooks executes the cleanupHost hooks from the settings snapshot in the state
// file. The watchdog cannot re-read the settings documents: they may have changed, or the
// project directory may be gone.
//
// In the watchdog path these scripts run without a TTY; output goes to the run log.
func runCleanupHostHooks(host hostenv.Host, instance *state.Instance) {
	if len(instance.Settings) == 0 {
		return
	}
	var document config.Document
	if err := json.Unmarshal(instance.Settings, &document); err != nil {
		logging.Warn("could not read the settings snapshot of %s, skipping cleanupHost hooks: %v",
			instance.InstanceName, err)
		return
	}
	settings, err := config.Decode(document)
	if err != nil {
		logging.Warn("could not decode the settings snapshot of %s, skipping cleanupHost hooks: %v",
			instance.InstanceName, err)
		return
	}
	scripts := hooks.Resolve(settings.Hooks.CleanupHost, host, instance.ProjectPath, "cleanupHost")
	hooks.RunCleanupHost(scripts, hookEnvironment(host, instance))
}

// verifyRemoved is the final check: anything still matching this instance is reported with
// the command that cleans it up, rather than left to be discovered later.
func verifyRemoved(containerEngine *engine.Engine, instance *state.Instance) {
	var leftovers []string
	// The runtime's name filter matches substrings, so confirm each hit really belongs to
	// this instance — a false positive here would print a spurious cleanup warning.
	for _, container := range containerEngine.Containers(instance.InstanceName) {
		if instanceOfContainer(container.Name) != instance.InstanceName {
			continue
		}
		leftovers = append(leftovers, "container "+container.Name)
	}
	for _, name := range instance.Networks {
		if containerEngine.NetworkExists(name) {
			leftovers = append(leftovers, "network "+name)
		}
	}
	if instance.DinDVolume != "" && containerEngine.VolumeExists(instance.DinDVolume) {
		leftovers = append(leftovers, "volume "+instance.DinDVolume)
	}
	if len(leftovers) == 0 {
		logging.Debug("instance %s fully removed", instance.InstanceName)
		return
	}
	logging.Warn("sandbox %s left resources behind: %v", instance.InstanceName, leftovers)
	logging.Warn("remove them with: hole destroy %s", instance.ProjectPath)
}

// lockInstance takes the per-instance teardown lock, blocking until it is available. A
// failure to lock is not fatal — teardown is idempotent, and refusing to clean up because a
// lock file could not be created would be worse than a redundant pass.
func lockInstance(path string) (func(), bool) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		logging.Debug("could not open teardown lock %s: %v", path, err)
		return func() {}, false
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		logging.Debug("could not acquire teardown lock %s: %v", path, err)
		_ = file.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, true
}

// hookEnvironment is the environment Hole exports to host-side hook scripts.
func hookEnvironment(host hostenv.Host, instance *state.Instance) []string {
	env := []string{
		"HOLE_PROJECT_DIR=" + instance.ProjectPath,
		"HOLE_PROJECT_NAME=" + instance.ProjectName,
		"HOLE_INSTANCE_NAME=" + instance.InstanceName,
		"HOLE_INSTANCE_ID=" + instance.InstanceID,
		"PROJECT_DIR=" + instance.ProjectPath,
		"PROJECT_NAME=" + instance.ProjectName,
		"SANDBOX_USERNAME=" + host.Username,
		"SANDBOX_HOME=" + host.Home,
	}
	if host.UID != "" {
		env = append(env, "SANDBOX_UID="+host.UID, "SANDBOX_GID="+host.GID)
	}
	if len(instance.Networks) > 0 {
		env = append(env, "HOLE_SANDBOX_NETWORK="+instance.Networks[0])
	}
	return env
}
