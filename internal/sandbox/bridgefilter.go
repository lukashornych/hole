package sandbox

import (
	"fmt"
	"strings"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

// bridgeNetfilterHelper is where the gateway Dockerfile installs the helper script.
const bridgeNetfilterHelper = "/usr/local/bin/hole-bridge-netfilter"

// installedMarker prefixes the helper's machine-readable "rule is in place" line, followed by
// `backend=<iptables-*> physdev=<yes|no>` naming the variant it installed; anything else on
// a zero exit means the host does not filter bridged packets and no rule was installed.
const installedMarker = "HOLE_BRIDGE_NETFILTER=installed"

// Helper exit codes that prove the host is affected: 3 = no DOCKER-USER chain to put the
// rule in, 4 = insertion failed. Both mean the sandbox would run with dead egress, so they
// fail the start; any other failure leaves the question open and only warns.
const (
	exitNoDockerUserChain = 3
	exitInsertFailed      = 4
)

const bridgeNetfilterGuidance = `This host pushes bridged packets through iptables (net.bridge.bridge-nf-call-iptables=1),
where Docker's internal-network rules drop the sandbox's egress before it reaches the gateway.
Hole could not repair it automatically. Manual options, narrowest first:
  - iptables -I DOCKER-USER -m physdev --physdev-is-bridged -j ACCEPT
    (affects only switched frames that never leave their bridge; Docker's filtering of routed
    traffic is untouched)
  - sysctl -w net.bridge.bridge-nf-call-iptables=0, persisted in /etc/sysctl.d/
    (host-wide; do NOT use it on a machine that is itself a Kubernetes node — kubeadm and
    host-mode k3s require the value to be 1)
Or set network.bridgeNetfilterFix to "off" in ~/.hole/settings.json to silence this check.`

// applyBridgeNetfilterFix restores same-bridge traffic on hosts where br_netfilter would
// otherwise drop it (see documentation/analysis/br-netfilter-internal-network-egress.md).
// It runs the helper baked into the gateway image inside the daemon's network namespace;
// the helper sweeps rules whose bridge no longer exists, reads the sysctl there, and
// installs this sandbox's rule only when the host actually filters bridged packets.
//
// A host proven to be affected but unfixable fails the start — the agent would only hang on
// every connection — while an inconclusive helper failure warns and continues, so an exotic
// engine that cannot run host-network containers does not lose sandboxes that would work.
func applyBridgeNetfilterFix(containerEngine *engine.Engine, store *state.Store,
	instance *state.Instance, settings *config.Settings, sandboxNetworkName, gatewayImage string) error {
	if settings.Network.BridgeNetfilterFix == "off" {
		logging.Debug("bridge netfilter fix disabled by settings")
		return nil
	}
	if !containerEngine.IsDocker() {
		// netavark writes different rules and podman support is on hold; revisit together.
		logging.Debug("bridge netfilter fix skipped on engine %s", containerEngine.Binary)
		return nil
	}

	networkID, err := containerEngine.NetworkID(sandboxNetworkName)
	if err != nil {
		logging.Warn("could not resolve the ID of network %s, skipping the br_netfilter check: %v",
			sandboxNetworkName, err)
		return nil
	}
	if len(networkID) < 12 {
		logging.Warn("network %s has unexpected ID %q, skipping the br_netfilter check",
			sandboxNetworkName, networkID)
		return nil
	}
	bridge := "br-" + networkID[:12]

	output, exitCode, err := containerEngine.RunHostHelper(gatewayImage, bridgeNetfilterHelper,
		"apply", bridge, instance.InstanceName)
	switch {
	case err == nil && strings.Contains(output, installedMarker):
		logging.Info("This host filters bridged packets (br_netfilter); restored sandbox egress with a "+
			"DOCKER-USER rule scoped to %s", bridge)
		instance.BridgeFilterRule = true
		instance.BridgeFilterBridge = bridge
		instance.BridgeFilterBackend, instance.BridgeFilterPhysdev = parseInstalledVariant(output)
		if err := store.Write(instance); err != nil {
			logging.Warn("could not update the instance registry: %v", err)
		}
		return nil
	case err == nil:
		logging.Debug("bridge netfilter fix not needed on this host")
		return nil
	case exitCode == exitNoDockerUserChain:
		return fmt.Errorf("sandbox egress cannot work on this host: bridge-nf-call-iptables is 1 and the "+
			"docker daemon has no DOCKER-USER iptables chain to repair it through\n%s", bridgeNetfilterGuidance)
	case exitCode == exitInsertFailed:
		return fmt.Errorf("sandbox egress cannot work on this host: bridge-nf-call-iptables is 1 and the "+
			"repair rule could not be inserted: %w\n%s", err, bridgeNetfilterGuidance)
	default:
		// The helper never ran to a verdict, so whether this host is affected is unknown.
		logging.Warn("could not check whether this host filters bridged packets: %v", err)
		logging.Warn("if every connection from the sandbox hangs, see the guidance below")
		logging.Warn("%s", bridgeNetfilterGuidance)
		return nil
	}
}

// parseInstalledVariant reads backend and physdev off the helper's installed line. A line an
// older helper printed bare falls back to the historical defaults the guidance always named.
func parseInstalledVariant(output string) (backend string, physdev bool) {
	backend, physdev = "iptables", true
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, installedMarker) {
			continue
		}
		for _, field := range strings.Fields(line) {
			switch {
			case strings.HasPrefix(field, "backend="):
				backend = strings.TrimPrefix(field, "backend=")
			case strings.HasPrefix(field, "physdev="):
				physdev = strings.TrimPrefix(field, "physdev=") == "yes"
			}
		}
		return backend, physdev
	}
	return backend, physdev
}

// removeBridgeNetfilterRule deletes the rule applyBridgeNetfilterFix installed. Best-effort:
// a rule that stays behind is inert (its bridge died with the network) and the next start's
// sweep collects it, so every failure only warns — but names what was left and how to remove
// it by hand, in the exact variant and through the exact backend the rule was installed with.
func removeBridgeNetfilterRule(containerEngine *engine.Engine, instance *state.Instance) {
	if !instance.BridgeFilterRule {
		return
	}
	manual := manualRemovalCommand(instance)
	if instance.GatewayImage == "" || !containerEngine.ImageExists(instance.GatewayImage) {
		logging.Warn("gateway image of %s is gone; its DOCKER-USER rule stays until the next start "+
			"sweeps it, or remove it manually with: %s", instance.InstanceName, manual)
		return
	}
	if _, _, err := containerEngine.RunHostHelper(instance.GatewayImage, bridgeNetfilterHelper,
		"remove", instance.InstanceName); err != nil {
		logging.Warn("could not remove the DOCKER-USER rule of %s (it is inert and the next start "+
			"sweeps it, or remove it manually with: %s): %v", instance.InstanceName, manual, err)
	}
}

// manualRemovalCommand is the `iptables -D` invocation that matches the rule the helper
// actually installed for this instance.
func manualRemovalCommand(instance *state.Instance) string {
	backend, physdev := instance.BridgeFilterBackend, instance.BridgeFilterPhysdev
	if backend == "" {
		backend, physdev = "iptables", true
	}
	physdevMatch := ""
	if physdev {
		physdevMatch = "-m physdev --physdev-is-bridged "
	}
	return fmt.Sprintf("%s -D DOCKER-USER -i %s -o %s %s-m comment --comment %s -j ACCEPT",
		backend, instance.BridgeFilterBridge, instance.BridgeFilterBridge, physdevMatch,
		instance.InstanceName)
}
