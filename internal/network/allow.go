// Package network owns everything about sandbox egress: the allow-list model, the
// generated gateway configuration (CoreDNS, dnsmasq, nftables) and per-instance subnet
// allocation.
package network

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// DefaultPorts apply to an allow entry that does not name any ports.
var DefaultPorts = []int{80, 443}

// DockerHubToken is the allow-list host that opts a sandbox into the Docker Hub image cache
// (`internal/dindregistry`). It counts as an exact entry (`docker.io`) or as a wildcard
// (`*.docker.io`); no other spelling does, because a capability users cannot spell is one they
// cannot reason about.
const DockerHubToken = "docker.io"

// Kind classifies an allow-list entry's host part.
type Kind string

const (
	// KindExact allows exactly one domain name.
	KindExact Kind = "exact"
	// KindWildcard allows subdomains of a domain, but not the domain itself.
	KindWildcard Kind = "wildcard"
	// KindIP allows one literal IPv4 address.
	KindIP Kind = "ip"
	// KindCIDR allows an IPv4 range.
	KindCIDR Kind = "cidr"
)

var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// Entry is one parsed allow-list rule: a host matcher plus the ports it opens. Ports apply
// to TCP and UDP alike.
type Entry struct {
	Kind Kind
	// Host is the domain (without the `*.` prefix for wildcards), the IP, or the CIDR.
	Host  string
	Ports []int
}

// String renders the entry back into shorthand form.
func (e Entry) String() string {
	host := e.Host
	if e.Kind == KindWildcard {
		host = "*." + host
	}
	if len(e.Ports) == 0 {
		return host
	}
	parts := make([]string, 0, len(e.Ports))
	for _, port := range e.Ports {
		parts = append(parts, strconv.Itoa(port))
	}
	return host + ":" + strings.Join(parts, ",")
}

// ParseEntry parses one shorthand allow-list entry: `<host>[:<port>[,<port>...]]`.
func ParseEntry(raw string) (Entry, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Entry{}, fmt.Errorf("empty allow entry")
	}

	hostPart, portPart := value, ""
	hasPorts := false
	// A CIDR has no ports unless a colon follows the prefix length, so splitting on the
	// last colon is safe for every accepted host form (IPv6 is not supported).
	if idx := strings.LastIndex(value, ":"); idx >= 0 {
		hostPart, portPart, hasPorts = value[:idx], value[idx+1:], true
	}

	ports := DefaultPorts
	if hasPorts {
		parsed, err := parsePorts(portPart)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid allow entry '%s': %w", raw, err)
		}
		ports = parsed
	}

	kind, host, err := parseHost(hostPart)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid allow entry '%s': %w", raw, err)
	}
	return Entry{Kind: kind, Host: host, Ports: ports}, nil
}

func parseHost(hostPart string) (Kind, string, error) {
	if hostPart == "" {
		return "", "", fmt.Errorf("missing host")
	}
	if strings.Contains(hostPart, "/") {
		prefix, err := netip.ParsePrefix(hostPart)
		if err != nil {
			return "", "", fmt.Errorf("not a valid IPv4 CIDR: %s", hostPart)
		}
		if !prefix.Addr().Is4() {
			return "", "", fmt.Errorf("only IPv4 CIDRs are supported: %s", hostPart)
		}
		return KindCIDR, prefix.Masked().String(), nil
	}
	if addr, err := netip.ParseAddr(hostPart); err == nil {
		if !addr.Is4() {
			return "", "", fmt.Errorf("only IPv4 addresses are supported: %s", hostPart)
		}
		return KindIP, addr.String(), nil
	}
	if rest, ok := strings.CutPrefix(hostPart, "*."); ok {
		if !domainPattern.MatchString(rest) {
			return "", "", fmt.Errorf("not a valid wildcard domain: %s", hostPart)
		}
		return KindWildcard, strings.ToLower(rest), nil
	}
	if strings.Contains(hostPart, "*") {
		return "", "", fmt.Errorf("wildcards are only supported as a leading '*.' label: %s", hostPart)
	}
	if !domainPattern.MatchString(hostPart) {
		return "", "", fmt.Errorf("not a valid domain, IP or CIDR: %s", hostPart)
	}
	return KindExact, strings.ToLower(hostPart), nil
}

func parsePorts(raw string) ([]int, error) {
	fields := strings.Split(raw, ",")
	ports := make([]int, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, fmt.Errorf("empty port")
		}
		port, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("port '%s' is not a number", field)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("port %d out of range 1-65535", port)
		}
		ports = append(ports, port)
	}
	return normalizePorts(ports), nil
}

func normalizePorts(ports []int) []int {
	sorted := append([]int(nil), ports...)
	sort.Ints(sorted)
	out := make([]int, 0, len(sorted))
	for i, port := range sorted {
		if i > 0 && sorted[i-1] == port {
			continue
		}
		out = append(out, port)
	}
	return out
}

// ParseAllowFile parses an agent's allow.txt: one entry per line, `#` comments and blank
// lines ignored.
func ParseAllowFile(content []byte, label string) ([]Entry, error) {
	var entries []Entry
	for lineNo, line := range strings.Split(string(content), "\n") {
		text := line
		if idx := strings.Index(text, "#"); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		entry, err := ParseEntry(text)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", label, lineNo+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// HostGatewayDomain is a domain resolved to the Docker host gateway address so the sandbox
// can reach services running on the host.
type HostGatewayDomain struct {
	Domain string
	// Ports restricts the firewall allow; nil means every port, which is the historical
	// behavior for these explicitly user-configured host services.
	Ports []int
}

// ParseHostGatewayDomain validates one `network.hostGatewayDomains` entry:
// `<domain>[:<port>[,<port>...]]`. Without a port suffix every port is allowed, which is the
// historical behavior — host services are explicitly user-configured and typically listen on
// arbitrary development ports.
func ParseHostGatewayDomain(raw string) (HostGatewayDomain, error) {
	value := strings.TrimSpace(raw)

	domainPart, portPart := value, ""
	hasPorts := false
	if idx := strings.LastIndex(value, ":"); idx >= 0 {
		domainPart, portPart, hasPorts = value[:idx], value[idx+1:], true
	}
	if !domainPattern.MatchString(domainPart) {
		return HostGatewayDomain{}, fmt.Errorf(
			"invalid hostGatewayDomains entry: '%s' — must be a valid domain name with an optional :port,port suffix", raw)
	}

	entry := HostGatewayDomain{Domain: strings.ToLower(domainPart)}
	if hasPorts {
		ports, err := parsePorts(portPart)
		if err != nil {
			return HostGatewayDomain{}, fmt.Errorf("invalid hostGatewayDomains entry '%s': %w", raw, err)
		}
		entry.Ports = ports
	}
	return entry, nil
}

// Group is a set of hosts sharing one port set. Each group becomes one nftables set pair
// plus the dnsmasq lines that populate it.
type Group struct {
	// Name is the nftables set name (`g0`, `g1`, ...).
	Name  string
	Ports []int
	// Domains are exact and wildcard base names; dnsmasq matching is suffix-wide, which is
	// harmless because CoreDNS only forwards names the policy already approved.
	Domains []string
	// Statics are literal IPs and CIDRs, loaded into a static interval set.
	Statics []string
}

// Policy is the fully resolved egress policy for one sandbox run.
type Policy struct {
	// Exact and Wildcards drive the CoreDNS policy regex.
	Exact     []string
	Wildcards []string
	Groups    []Group
	// HostGateway domains are answered by CoreDNS with the host gateway address.
	HostGateway []HostGatewayDomain
	// Unrestricted disables filtering entirely (-u): DNS forwards everything and the
	// firewall's forward chain accepts.
	Unrestricted bool
}

// AllowsDockerHub reports whether this policy opts in to the Docker Hub image cache, i.e.
// whether it carries DockerHubToken as an exact or wildcard host. Unrestricted mode allows every
// host, so it opts in too.
//
// The resolved policy is the input rather than the raw setting, so port suffixes, padding, casing
// and an agent's own allow.txt are all already accounted for.
func (p Policy) AllowsDockerHub() bool {
	return p.Unrestricted ||
		slices.Contains(p.Exact, DockerHubToken) ||
		slices.Contains(p.Wildcards, DockerHubToken)
}

// BuildPolicy folds allow entries into a deterministic policy: entries for the same host
// merge their ports, hosts are grouped by identical port sets, and groups are named in a
// stable order so generated artifacts are reproducible.
func BuildPolicy(entries []Entry, hostGateway []HostGatewayDomain, unrestricted bool) Policy {
	type hostKey struct {
		kind Kind
		host string
	}
	portsByHost := map[hostKey][]int{}
	var order []hostKey
	for _, entry := range entries {
		key := hostKey{kind: entry.Kind, host: entry.Host}
		if _, seen := portsByHost[key]; !seen {
			order = append(order, key)
		}
		portsByHost[key] = normalizePorts(append(portsByHost[key], entry.Ports...))
	}

	policy := Policy{HostGateway: mergeHostGateway(hostGateway), Unrestricted: unrestricted}

	groupsByPorts := map[string]*Group{}
	var groupKeys []string
	for _, key := range order {
		ports := portsByHost[key]
		if len(ports) == 0 {
			continue
		}
		portKey := portsKey(ports)
		group, ok := groupsByPorts[portKey]
		if !ok {
			group = &Group{Ports: ports}
			groupsByPorts[portKey] = group
			groupKeys = append(groupKeys, portKey)
		}
		switch key.kind {
		case KindExact:
			policy.Exact = append(policy.Exact, key.host)
			group.Domains = append(group.Domains, key.host)
		case KindWildcard:
			policy.Wildcards = append(policy.Wildcards, key.host)
			group.Domains = append(group.Domains, key.host)
		case KindIP, KindCIDR:
			group.Statics = append(group.Statics, key.host)
		}
	}

	sort.Strings(groupKeys)
	sort.Strings(policy.Exact)
	sort.Strings(policy.Wildcards)
	policy.Exact = dedup(policy.Exact)
	policy.Wildcards = dedup(policy.Wildcards)

	for i, portKey := range groupKeys {
		group := groupsByPorts[portKey]
		group.Name = fmt.Sprintf("g%d", i)
		sort.Strings(group.Domains)
		sort.Strings(group.Statics)
		group.Domains = dedup(group.Domains)
		group.Statics = dedup(group.Statics)
		policy.Groups = append(policy.Groups, *group)
	}
	return policy
}

// mergeHostGateway folds entries for the same domain into one, unioning their ports. CoreDNS
// refuses a Corefile that defines the same zone twice, so two entries for one domain would kill
// the gateway at startup — and merging is what the nftables side already does anyway. A
// port-less entry means every port, so it absorbs any port list for the same domain.
func mergeHostGateway(entries []HostGatewayDomain) []HostGatewayDomain {
	merged := map[string]HostGatewayDomain{}
	var domains []string
	for _, entry := range entries {
		existing, seen := merged[entry.Domain]
		if !seen {
			domains = append(domains, entry.Domain)
			first := HostGatewayDomain{Domain: entry.Domain}
			if len(entry.Ports) > 0 {
				first.Ports = normalizePorts(entry.Ports)
			}
			merged[entry.Domain] = first
			continue
		}
		if len(existing.Ports) == 0 || len(entry.Ports) == 0 {
			existing.Ports = nil
		} else {
			existing.Ports = normalizePorts(append(existing.Ports, entry.Ports...))
		}
		merged[entry.Domain] = existing
	}

	sort.Strings(domains)
	out := make([]HostGatewayDomain, 0, len(domains))
	for _, domain := range domains {
		out = append(out, merged[domain])
	}
	return out
}

func portsKey(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%05d", port))
	}
	return strings.Join(parts, ",")
}

func dedup(values []string) []string {
	out := make([]string, 0, len(values))
	for i, value := range values {
		if i > 0 && values[i-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}
