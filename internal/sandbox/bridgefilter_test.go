package sandbox

import (
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/state"
)

// Both gates must skip without an error and without recording a rule. Neither subtest can
// reach a real installation: "off" returns at the settings gate (and even a regression there
// would stop at the inspect of a network that does not exist), and a non-docker engine
// returns after at most a `--version` flavor probe.
func TestApplyBridgeNetfilterFixSkips(t *testing.T) {
	tests := map[string]struct {
		binary   string
		settings config.Settings
	}{
		"disabled by settings": {
			binary:   "docker",
			settings: config.Settings{Network: config.NetworkSettings{BridgeNetfilterFix: "off"}},
		},
		"non-docker engine": {
			binary:   "podman",
			settings: config.Settings{},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			containerEngine := &engine.Engine{Binary: test.binary}
			_, store := newTestStore(t)
			instance := &state.Instance{InstanceName: "hole-sandbox-p-1"}
			if err := applyBridgeNetfilterFix(containerEngine, store, instance, &test.settings,
				"hole-sandbox-p-1_sandbox", "img"); err != nil {
				t.Errorf("skip path returned an error: %v", err)
			}
			if instance.BridgeFilterRule {
				t.Error("skip path must not record an installed rule")
			}
		})
	}
}

func TestRemoveBridgeNetfilterRuleIsANoOpWithoutARule(t *testing.T) {
	// An engine whose binary does not exist proves nothing is invoked.
	removeBridgeNetfilterRule(&engine.Engine{Binary: "docker-that-does-not-exist"},
		&state.Instance{InstanceName: "hole-sandbox-p-1"})
}

func TestParseInstalledVariant(t *testing.T) {
	tests := map[string]struct {
		output      string
		wantBackend string
		wantPhysdev bool
	}{
		"nft with physdev": {
			output:      "HOLE_BRIDGE_NETFILTER=installed backend=iptables-nft physdev=yes\n",
			wantBackend: "iptables-nft",
			wantPhysdev: true,
		},
		"legacy without physdev": {
			output:      "noise\nHOLE_BRIDGE_NETFILTER=installed backend=iptables-legacy physdev=no\n",
			wantBackend: "iptables-legacy",
			wantPhysdev: false,
		},
		"bare marker falls back to the historical defaults": {
			output:      "HOLE_BRIDGE_NETFILTER=installed\n",
			wantBackend: "iptables",
			wantPhysdev: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			backend, physdev := parseInstalledVariant(test.output)
			if backend != test.wantBackend || physdev != test.wantPhysdev {
				t.Errorf("parseInstalledVariant(%q) = %q, %v; want %q, %v",
					test.output, backend, physdev, test.wantBackend, test.wantPhysdev)
			}
		})
	}
}

// The command the teardown warning names must match the rule variant that was actually
// installed — a physdev command for a no-physdev rule reports "Bad rule" to the user.
func TestManualRemovalCommandNamesTheInstalledVariant(t *testing.T) {
	instance := &state.Instance{
		InstanceName:        "hole-sandbox-p-1",
		BridgeFilterBridge:  "br-abcdef012345",
		BridgeFilterBackend: "iptables-legacy",
		BridgeFilterPhysdev: false,
	}
	want := "iptables-legacy -D DOCKER-USER -i br-abcdef012345 -o br-abcdef012345 " +
		"-m comment --comment hole-sandbox-p-1 -j ACCEPT"
	if got := manualRemovalCommand(instance); got != want {
		t.Errorf("manualRemovalCommand = %q, want %q", got, want)
	}

	instance.BridgeFilterBackend = "iptables-nft"
	instance.BridgeFilterPhysdev = true
	want = "iptables-nft -D DOCKER-USER -i br-abcdef012345 -o br-abcdef012345 " +
		"-m physdev --physdev-is-bridged -m comment --comment hole-sandbox-p-1 -j ACCEPT"
	if got := manualRemovalCommand(instance); got != want {
		t.Errorf("manualRemovalCommand = %q, want %q", got, want)
	}
}
