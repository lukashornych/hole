package network

import (
	"reflect"
	"regexp"
	"testing"
)

func TestParseEntry(t *testing.T) {
	tests := []struct {
		raw   string
		kind  Kind
		host  string
		ports []int
	}{
		{"api.github.com", KindExact, "api.github.com", []int{80, 443}},
		{"*.npmjs.org", KindWildcard, "npmjs.org", []int{80, 443}},
		{"db.example.com:5432", KindExact, "db.example.com", []int{5432}},
		{"github.com:22,443", KindExact, "github.com", []int{22, 443}},
		{"10.0.0.5:22,2222", KindIP, "10.0.0.5", []int{22, 2222}},
		{"192.168.1.0/24:8080", KindCIDR, "192.168.1.0/24", []int{8080}},
		{"10.0.0.0/8", KindCIDR, "10.0.0.0/8", []int{80, 443}},
		{"EXAMPLE.COM", KindExact, "example.com", []int{80, 443}},
		{"localhost", KindExact, "localhost", []int{80, 443}},
		{"  spaced.example.com  ", KindExact, "spaced.example.com", []int{80, 443}},
		{"*.a.b.c.example.com:9000", KindWildcard, "a.b.c.example.com", []int{9000}},
		{"host.example.com:443,443", KindExact, "host.example.com", []int{443}},
		{"192.168.1.5/32", KindCIDR, "192.168.1.5/32", []int{80, 443}},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			entry, err := ParseEntry(test.raw)
			if err != nil {
				t.Fatalf("ParseEntry(%q): %v", test.raw, err)
			}
			if entry.Kind != test.kind || entry.Host != test.host {
				t.Errorf("ParseEntry(%q) = %s/%s, want %s/%s", test.raw, entry.Kind, entry.Host, test.kind, test.host)
			}
			if !reflect.DeepEqual(entry.Ports, test.ports) {
				t.Errorf("ParseEntry(%q) ports = %v, want %v", test.raw, entry.Ports, test.ports)
			}
		})
	}
}

func TestParseEntryRejectsMalformed(t *testing.T) {
	invalid := []string{
		"",
		"  ",
		"example..com",
		"-example.com",
		"example.com:",
		"example.com:0",
		"example.com:65536",
		"example.com:https",
		"example.com:80,",
		"ex*mple.com",
		"*example.com",
		"*.",
		"10.0.0.5/33",
		"::1",
		"2001:db8::/32",
		"example.com:80:443",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if entry, err := ParseEntry(raw); err == nil {
				t.Errorf("ParseEntry(%q) accepted as %+v", raw, entry)
			}
		})
	}
}

func TestEntryStringRoundTrip(t *testing.T) {
	for _, raw := range []string{"api.github.com:80,443", "*.npmjs.org:80,443", "10.0.0.5:22", "10.0.0.0/8:80,443"} {
		entry, err := ParseEntry(raw)
		if err != nil {
			t.Fatalf("ParseEntry(%q): %v", raw, err)
		}
		if got := entry.String(); got != raw {
			t.Errorf("round trip of %q produced %q", raw, got)
		}
	}
}

func TestParseAllowFile(t *testing.T) {
	content := []byte(`# a comment
api.anthropic.com

claude.ai        # trailing comment
*.claude.ai
`)
	entries, err := ParseAllowFile(content, "test")
	if err != nil {
		t.Fatalf("ParseAllowFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(entries), entries)
	}
	if entries[2].Kind != KindWildcard || entries[2].Host != "claude.ai" {
		t.Errorf("wildcard entry parsed as %+v", entries[2])
	}
}

func TestParseAllowFileReportsLineNumbers(t *testing.T) {
	_, err := ParseAllowFile([]byte("good.example.com\nbad..entry\n"), "agent 'x' allow.txt")
	if err == nil {
		t.Fatal("malformed allow file accepted")
	}
	if want := "line 2"; !contains(err.Error(), want) {
		t.Errorf("error %q does not name the offending line", err)
	}
}

func TestParseHostGatewayDomain(t *testing.T) {
	entry, err := ParseHostGatewayDomain("mydb.local")
	if err != nil {
		t.Fatalf("valid host gateway domain rejected: %v", err)
	}
	// No port suffix means every port, which is the historical behavior for host services.
	if entry.Domain != "mydb.local" || entry.Ports != nil {
		t.Errorf("ParseHostGatewayDomain = %+v, want mydb.local with no port restriction", entry)
	}

	withPorts, err := ParseHostGatewayDomain("mydb.local:5432,8080")
	if err != nil {
		t.Fatalf("port suffix rejected: %v", err)
	}
	if !reflect.DeepEqual(withPorts.Ports, []int{5432, 8080}) {
		t.Errorf("ports = %v, want [5432 8080]", withPorts.Ports)
	}

	for _, raw := range []string{"my db", "", "-bad.local", "mydb.local:", "mydb.local:0", "mydb.local:https"} {
		if _, err := ParseHostGatewayDomain(raw); err == nil {
			t.Errorf("invalid host gateway domain %q accepted", raw)
		}
	}
}

func TestBuildPolicyGroupsByPortSet(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "a.example.com", Ports: []int{80, 443}},
		{Kind: KindWildcard, Host: "b.example.com", Ports: []int{80, 443}},
		{Kind: KindExact, Host: "db.example.com", Ports: []int{5432}},
		{Kind: KindCIDR, Host: "10.0.0.0/24", Ports: []int{5432}},
	}
	policy := BuildPolicy(entries, nil, false)

	if len(policy.Groups) != 2 {
		t.Fatalf("expected 2 port groups, got %d: %+v", len(policy.Groups), policy.Groups)
	}
	// Groups are keyed by port set and named deterministically.
	if policy.Groups[0].Name != "g0" || policy.Groups[1].Name != "g1" {
		t.Errorf("group names are not stable: %+v", policy.Groups)
	}
	if !reflect.DeepEqual(policy.Exact, []string{"a.example.com", "db.example.com"}) {
		t.Errorf("exact names = %v", policy.Exact)
	}
	if !reflect.DeepEqual(policy.Wildcards, []string{"b.example.com"}) {
		t.Errorf("wildcards = %v", policy.Wildcards)
	}
}

func TestBuildPolicyMergesPortsOfRepeatedHost(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "github.com", Ports: []int{443}},
		{Kind: KindExact, Host: "github.com", Ports: []int{22}},
	}
	policy := BuildPolicy(entries, nil, false)
	if len(policy.Groups) != 1 {
		t.Fatalf("expected one group, got %+v", policy.Groups)
	}
	if !reflect.DeepEqual(policy.Groups[0].Ports, []int{22, 443}) {
		t.Errorf("ports of repeated host = %v, want [22 443]", policy.Groups[0].Ports)
	}
}

// Two entries for one domain used to reach the Corefile verbatim, which CoreDNS rejects with
// "zone already defined" — killing the gateway and with it every start.
func TestBuildPolicyMergesHostGatewayDomains(t *testing.T) {
	tests := []struct {
		name    string
		entries []HostGatewayDomain
		want    []HostGatewayDomain
	}{
		{
			name:    "ports union",
			entries: []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080}}, {Domain: "app.test", Ports: []int{9090}}},
			want:    []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080, 9090}}},
		},
		{
			name:    "port-less wins over a port list",
			entries: []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080}}, {Domain: "app.test"}},
			want:    []HostGatewayDomain{{Domain: "app.test"}},
		},
		{
			name:    "port-less first still wins",
			entries: []HostGatewayDomain{{Domain: "app.test"}, {Domain: "app.test", Ports: []int{8080}}},
			want:    []HostGatewayDomain{{Domain: "app.test"}},
		},
		{
			name:    "distinct domains are sorted",
			entries: []HostGatewayDomain{{Domain: "z.test"}, {Domain: "a.test", Ports: []int{80}}},
			want:    []HostGatewayDomain{{Domain: "a.test", Ports: []int{80}}, {Domain: "z.test"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := BuildPolicy(nil, test.entries, false)
			if !reflect.DeepEqual(policy.HostGateway, test.want) {
				t.Fatalf("HostGateway = %+v, want %+v", policy.HostGateway, test.want)
			}
			artifacts, err := policy.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			for _, entry := range test.want {
				if occurrences(artifacts.Corefile, entry.Domain+":53 {") != 1 {
					t.Errorf("Corefile defines zone %s more than once:\n%s", entry.Domain, artifacts.Corefile)
				}
			}
		})
	}
}

func TestBuildPolicyIsDeterministic(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "z.example.com", Ports: []int{443}},
		{Kind: KindExact, Host: "a.example.com", Ports: []int{443}},
		{Kind: KindIP, Host: "10.0.0.9", Ports: []int{443}},
	}
	first := BuildPolicy(entries, nil, false)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(BuildPolicy(entries, nil, false), first) {
			t.Fatal("BuildPolicy is not deterministic")
		}
	}
}

func TestPolicyRegex(t *testing.T) {
	policy := BuildPolicy([]Entry{
		{Kind: KindExact, Host: "api.github.com", Ports: []int{443}},
		{Kind: KindWildcard, Host: "npmjs.org", Ports: []int{443}},
	}, nil, false)

	regex := policy.PolicyRegex()
	matcher := mustCompile(t, regex)

	allowed := []string{"api.github.com", "api.github.com.", "API.GITHUB.COM", "registry.npmjs.org", "a.b.npmjs.org."}
	for _, name := range allowed {
		if !matcher.MatchString(name) {
			t.Errorf("policy regex %q should match %q", regex, name)
		}
	}
	// The wildcard must not cover the apex, and unrelated names must not match.
	denied := []string{"npmjs.org", "evil-api.github.com", "api.github.com.evil.com", "github.com"}
	for _, name := range denied {
		if matcher.MatchString(name) {
			t.Errorf("policy regex %q must not match %q", regex, name)
		}
	}
}

func TestPolicyRegexEmptyWhenNothingAllowed(t *testing.T) {
	if got := BuildPolicy(nil, nil, false).PolicyRegex(); got != "" {
		t.Errorf("expected empty regex for empty policy, got %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func occurrences(haystack, needle string) int {
	count := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			count++
		}
	}
	return count
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("generated policy regex does not compile: %v", err)
	}
	return compiled
}
