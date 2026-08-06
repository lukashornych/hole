package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lukashornych/hole/internal/engine"
	"github.com/lukashornych/hole/internal/hostenv"
	"github.com/lukashornych/hole/internal/logging"
	"github.com/lukashornych/hole/internal/network"
	"github.com/lukashornych/hole/internal/state"
)

// coreDNSQuery matches the CoreDNS log plugin's query lines, e.g.
//
//	[INFO] 10.222.0.3:41234 - 29963 "A IN example.com. udp 29 false 512" NOERROR qr,rd,ra 60 0.001s
//
// The name and the response code are all the dump needs: NXDOMAIN means the policy refused
// to resolve the name, anything else means the sandbox was allowed to reach it.
var coreDNSQuery = regexp.MustCompile(`"[A-Z]+ [A-Z]+ ([^ "]+)[^"]*" ([A-Z]+)`)

// writeNetworkAccessDump extracts the domains the sandbox resolved (and those it was
// refused) from the gateway's DNS log.
//
// The dump is written under `~/.hole/logs/<project>/`, never into the project's own
// `.hole/logs`: that directory is bind-mounted read-write with the host UID, so the sandbox
// could replace it with a symlink and turn this host-side write into an arbitrary-file
// overwrite as the invoking user. `~/.hole` is outside every sandbox mount.
//
// Direct-IP connection attempts blocked by the firewall do not appear here — they never
// produce a DNS query. The nftables denied counter records them for debugging.
func writeNetworkAccessDump(containerEngine *engine.Engine, host hostenv.Host, instance *state.Instance) {
	container := instance.InstanceName + "-gateway-1"
	logs, err := containerEngine.ContainerLogs(container)
	if err != nil && strings.TrimSpace(logs) == "" {
		logging.Line()
		logging.Warn("could not retrieve DNS log from the gateway container")
		return
	}

	entries := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		match := coreDNSQuery.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		name := strings.TrimSuffix(match[1], ".")
		if name == "" || name == network.HealthZone {
			continue
		}
		verdict := "ALLOWED"
		if match[2] == "NXDOMAIN" {
			verdict = "DENIED"
		}
		entries[verdict+" "+name] = true
	}

	lines := make([]string, 0, len(entries))
	for entry := range entries {
		lines = append(lines, entry)
	}
	sort.Strings(lines)

	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	logFile, err := writeDumpFile(host, instance, content)
	if err != nil {
		logging.Warn("could not write network access log: %v", err)
		return
	}
	logging.Line()
	logging.Info("Network access log written to: %s", logFile)
}

// writeDumpFile stores the dump under `~/.hole/logs/<project>/` and returns the path.
//
// The per-project directory lives in `~/.hole`, which is outside every sandbox mount, so the
// sandbox cannot pre-plant a symlink at the write target — unlike the project's own
// `.hole/logs`, which is bind-mounted read-write with the host UID.
func writeDumpFile(host hostenv.Host, instance *state.Instance, content string) (string, error) {
	logDir := filepath.Join(host.LogDir(), instance.ProjectName)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", logDir, err)
	}
	logFile := filepath.Join(logDir, fmt.Sprintf("network-access-%s-%s.log", instance.Agent, instance.InstanceID))
	if err := os.WriteFile(logFile, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", logFile, err)
	}
	return logFile, nil
}
