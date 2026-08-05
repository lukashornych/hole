package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MigrationURL is where the upgrade guide lives.
const MigrationURL = "https://github.com/lukashornych/hole/blob/main/MIGRATION.md"

// MigrationError reports settings keys that Hole 2.0 removed. It carries a paste-ready
// replacement so the user does not have to work out the translation themselves.
type MigrationError struct {
	Label   string
	Details []string
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("%s uses settings that were removed in Hole 2.0:\n%s", e.Label, strings.Join(e.Details, "\n"))
}

// CheckRemovedKeys runs before schema validation so removed keys produce a targeted
// migration error instead of a bare "additional properties not allowed".
//
// The 1.x → 2.0 translation lives here and only here: it generates a hint, never runtime
// behavior. Silently reinterpreting an old allow list would be the one outcome worse than
// failing — the user would believe a policy is in force that Hole never applied.
func CheckRemovedKeys(label string, document Document) error {
	if document == nil {
		return nil
	}
	var details []string

	if network, ok := document["network"].(map[string]any); ok {
		_, hasWhitelist := network["domainWhitelist"]
		portsValue, hasPorts := network["allowedPorts"]
		if hasWhitelist || hasPorts {
			details = append(details,
				"  network.domainWhitelist and network.allowedPorts have been replaced by network.allow,",
				"  which takes <host>[:<port>[,<port>...]] entries and filters every protocol and port.")
			details = append(details, migrationAllowHint(network["domainWhitelist"], portsValue, hasPorts)...)
		}
	}

	if hooks, ok := document["hooks"].(map[string]any); ok {
		if setup, isObject := hooks["setup"].(map[string]any); isObject {
			details = append(details,
				"  hooks.setup is now an array, so several scripts (and globs) can be baked into the image.")
			details = append(details, migrationSetupHint(setup)...)
		}
	}

	if len(details) == 0 {
		return nil
	}
	details = append(details, "  See "+MigrationURL+" for the full upgrade guide.")
	return &MigrationError{Label: label, Details: details}
}

// migrationAllowHint renders the network.allow block equivalent to the old keys.
//
// tinyproxy matched whitelist entries as unanchored regexes, so each domain covered its
// subdomains too; the hint keeps that reachability by suggesting both an exact and a
// wildcard entry.
func migrationAllowHint(whitelistValue, portsValue any, hasPorts bool) []string {
	ports, portsEmpty := migrationPorts(portsValue, hasPorts)

	var hosts []string
	if list, ok := whitelistValue.([]any); ok {
		for _, item := range list {
			domain, ok := item.(string)
			if !ok || strings.TrimSpace(domain) == "" {
				continue
			}
			hosts = append(hosts, strings.TrimSpace(domain))
		}
	}
	sort.Strings(hosts)

	if portsEmpty {
		return []string{
			"  Note: allowedPorts was empty, which used to block all traffic. Leave network.allow",
			"  empty to keep that, or list the hosts and ports the project actually needs.",
		}
	}
	if len(hosts) == 0 {
		return []string{
			"  Replace them with a network.allow array listing the hosts the project needs, e.g.:",
			`    "network": { "allow": ["api.github.com", "*.npmjs.org:443"] }`,
		}
	}

	hint := []string{"  Replace them with:", `    "network": {`, `      "allow": [`}
	var entries []string
	for _, host := range hosts {
		suffix := ""
		if ports != "" {
			suffix = ":" + ports
		}
		entries = append(entries, fmt.Sprintf(`        "%s%s"`, host, suffix))
		// An IP or CIDR has no subdomains, so no wildcard twin for those.
		if !looksLikeAddress(host) {
			entries = append(entries, fmt.Sprintf(`        "*.%s%s"`, host, suffix))
		}
	}
	hint = append(hint, strings.Join(entries, ",\n"))
	return append(hint, `      ]`, `    }`)
}

// migrationPorts renders the port suffix for the hint and reports the block-everything case.
func migrationPorts(portsValue any, hasPorts bool) (string, bool) {
	if !hasPorts {
		return "", false
	}
	list, ok := portsValue.([]any)
	if !ok {
		return "", false
	}
	if len(list) == 0 {
		return "", true
	}
	var ports []int
	for _, item := range list {
		switch typed := item.(type) {
		case float64:
			ports = append(ports, int(typed))
		case int:
			ports = append(ports, typed)
		}
	}
	sort.Ints(ports)
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, strconv.Itoa(port))
	}
	return strings.Join(parts, ","), false
}

// migrationSetupHint renders the array form of a scalar hooks.setup entry.
func migrationSetupHint(setup map[string]any) []string {
	script, _ := setup["script"].(string)
	if script == "" {
		script = ".hole/setup.sh"
	}
	return []string{
		"  Replace it with:",
		`    "hooks": { "setup": [{ "script": "` + script + `" }] }`,
	}
}

// looksLikeAddress reports whether an entry is an IPv4 address or CIDR rather than a domain.
func looksLikeAddress(host string) bool {
	if strings.Contains(host, "/") {
		return true
	}
	for _, part := range strings.Split(host, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}
