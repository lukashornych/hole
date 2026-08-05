package network

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden gateway artifacts")

func entries(t *testing.T, raws ...string) []Entry {
	t.Helper()
	out := make([]Entry, 0, len(raws))
	for _, raw := range raws {
		entry, err := ParseEntry(raw)
		if err != nil {
			t.Fatalf("ParseEntry(%q): %v", raw, err)
		}
		out = append(out, entry)
	}
	return out
}

func TestGenerateGoldenArtifacts(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
	}{
		{
			name: "default",
			policy: BuildPolicy(entries(t,
				"api.anthropic.com",
				"claude.ai",
				"*.claude.ai",
			), nil, false),
		},
		{
			name: "mixed",
			policy: BuildPolicy(entries(t,
				"api.github.com",
				"*.npmjs.org",
				"db.example.com:5432",
				"github.com:22,443",
				"10.0.0.5:22,2222",
				"192.168.1.0/24:8080",
			), []HostGatewayDomain{{Domain: "mydb.local"}}, false),
		},
		{
			name: "host-gateway-ports",
			policy: BuildPolicy(entries(t, "api.github.com"),
				[]HostGatewayDomain{{Domain: "mydb.local", Ports: []int{5432, 8080}}}, false),
		},
		{
			// An address covered by a CIDR in the same set: nft rejects the ruleset as
			// "conflicting intervals" unless the set is declared auto-merge.
			name: "overlapping-statics",
			policy: BuildPolicy(entries(t,
				"10.0.0.0/24:443",
				"10.0.0.5:443",
			), nil, false),
		},
		{
			// Two entries for one domain: CoreDNS refuses a Corefile that defines a zone twice.
			name: "duplicate-host-gateway",
			policy: BuildPolicy(entries(t, "api.github.com"), []HostGatewayDomain{
				{Domain: "app.test", Ports: []int{8080}},
				{Domain: "app.test", Ports: []int{9090}},
			}, false),
		},
		{
			name:   "empty",
			policy: BuildPolicy(nil, nil, false),
		},
		{
			name:   "unrestricted",
			policy: BuildPolicy(entries(t, "api.github.com"), nil, true),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifacts, err := test.policy.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			files := map[string]string{
				"Corefile":       artifacts.Corefile,
				"dnsmasq.conf":   artifacts.DnsmasqConf,
				"nftables.rules": artifacts.NftablesRule,
			}
			for name, content := range files {
				golden := filepath.Join("testdata", test.name, name)
				if *updateGolden {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("read golden %s: %v (run 'go test ./internal/network -update' to create it)", golden, err)
				}
				if content != string(want) {
					t.Errorf("%s does not match %s:\n--- got ---\n%s\n--- want ---\n%s", name, golden, content, want)
				}
			}
		})
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	policy := BuildPolicy(entries(t, "b.example.com:443", "a.example.com:443", "10.0.0.1:22"), nil, false)
	first, err := policy.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := policy.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatal("Generate is not deterministic")
		}
	}
}

func TestGeneratedNftablesDropsByDefault(t *testing.T) {
	artifacts, err := BuildPolicy(entries(t, "api.github.com"), nil, false).Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(artifacts.NftablesRule, "policy drop;") {
		t.Error("forward chain must default to drop")
	}
	if !contains(artifacts.NftablesRule, "meta nfproto ipv6 drop") {
		t.Error("IPv6 must be dropped")
	}
	if !contains(artifacts.NftablesRule, "masquerade") {
		t.Error("egress must be masqueraded")
	}
}

func TestGeneratedUnrestrictedAcceptsEverything(t *testing.T) {
	artifacts, err := BuildPolicy(entries(t, "api.github.com"), nil, true).Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(artifacts.NftablesRule, "policy accept;") {
		t.Error("unrestricted mode must accept in the forward chain")
	}
	if contains(artifacts.DnsmasqConf, "nftset=") {
		t.Error("unrestricted mode must not populate nftables sets")
	}
	if contains(artifacts.Corefile, "view allowed") {
		t.Error("unrestricted mode must not gate DNS")
	}
	if contains(artifacts.Corefile, "NXDOMAIN") {
		t.Error("unrestricted mode must not install the catch-all NXDOMAIN block")
	}
}

func TestGeneratedEmptyPolicyDeniesEverything(t *testing.T) {
	artifacts, err := BuildPolicy(nil, nil, false).Generate()
	if err != nil {
		t.Fatal(err)
	}
	if contains(artifacts.Corefile, "view allowed") {
		t.Error("an empty policy must not emit an allowed view")
	}
	if !contains(artifacts.Corefile, "NXDOMAIN") {
		t.Error("an empty policy must still answer NXDOMAIN")
	}
	if contains(artifacts.NftablesRule, "@g0") {
		t.Error("an empty policy must not reference any set")
	}
}

func TestGeneratedHealthZoneIsAlwaysResolvable(t *testing.T) {
	for _, unrestricted := range []bool{false, true} {
		artifacts, err := BuildPolicy(nil, nil, unrestricted).Generate()
		if err != nil {
			t.Fatal(err)
		}
		if !contains(artifacts.Corefile, HealthZone) {
			t.Errorf("health zone missing from Corefile (unrestricted=%v) — the healthcheck would never pass", unrestricted)
		}
		// A zone that answers only A SERVFAILs every AAAA query for the same name, and a bare
		// `nslookup <name>` asks both — which made the gateway permanently unhealthy.
		for _, recordType := range []string{"A", "AAAA"} {
			block := "template IN " + recordType + " " + HealthZone
			if !contains(artifacts.Corefile, block) {
				t.Errorf("health zone has no %s answer (unrestricted=%v): resolvers that ask for both record types fail",
					recordType, unrestricted)
			}
		}
	}
}

func TestGeneratedSetsMatchDnsmasqReferences(t *testing.T) {
	policy := BuildPolicy(entries(t, "a.example.com:443", "db.example.com:5432", "10.0.0.0/8:9000"), nil, false)
	artifacts, err := policy.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// Every set dnsmasq writes into must be declared in the ruleset, or nft rejects the
	// insert at runtime.
	for _, group := range policy.Groups {
		if len(group.Domains) == 0 {
			continue
		}
		if !contains(artifacts.DnsmasqConf, "#inet#hole#"+group.Name) {
			t.Errorf("dnsmasq does not populate set %s", group.Name)
		}
		if !contains(artifacts.NftablesRule, "set "+group.Name+" {") {
			t.Errorf("nftables does not declare set %s", group.Name)
		}
	}
	// A group holding only literal addresses gets a static set and no dnsmasq line.
	if !contains(artifacts.NftablesRule, "_static {") {
		t.Error("literal IP/CIDR entries must land in a static interval set")
	}
}
