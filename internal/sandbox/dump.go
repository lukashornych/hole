package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lukashornych/hole/internal/engine"
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
// Direct-IP connection attempts blocked by the firewall do not appear here — they never
// produce a DNS query. The nftables denied counter records them for debugging.
func writeNetworkAccessDump(containerEngine *engine.Engine, instance *state.Instance) {
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

	logDir := filepath.Join(instance.ProjectPath, ".hole", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		logging.Warn("could not create %s: %v", logDir, err)
		return
	}
	logFile := filepath.Join(logDir, fmt.Sprintf("network-access-%s-%s.log", instance.Agent, instance.InstanceID))
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(logFile, []byte(content), 0o644); err != nil {
		logging.Warn("could not write %s: %v", logFile, err)
		return
	}
	logging.Line()
	logging.Info("Network access log written to: %s", logFile)
}
