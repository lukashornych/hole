package network

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// HealthZone is the name the gateway healthcheck resolves. It is answered by a dedicated
// CoreDNS block so the probe does not depend on any user-configured domain. The block answers
// AAAA with an empty NOERROR as well: a zone that SERVFAILs one query type fails resolvers
// that ask for both, which is what a bare `nslookup <name>` does.
const HealthZone = "health.hole.internal"

// Artifacts are the three generated files the gateway container mounts read-only.
type Artifacts struct {
	Corefile     string
	DnsmasqConf  string
	NftablesRule string
}

const corefileTemplate = `{{ .HealthZone }}:53 {
    template IN A {{ .HealthZone }} {
        answer "{{ "{{ .Name }}" }} 0 IN A 127.0.0.1"
    }
    template IN AAAA {{ .HealthZone }} {
        rcode NOERROR
    }
    errors
}
{{ range .HostGateway }}
{{ .Domain }}:53 {
    template IN A {{ .Domain }} {
        answer "{{ "{{ .Name }}" }} 60 IN A {HOST_GATEWAY_IP}"
    }
    template IN AAAA {{ .Domain }} {
      rcode NOERROR
    }
    log
    errors
}
{{ end }}
{{- if .Unrestricted }}
. {
    forward . 127.0.0.1:5353
    log
    errors
}
{{- else }}
{{- if .PolicyRegex }}
. {
    view allowed {
        expr name() matches '{{ .PolicyRegex }}'
    }
    forward . 127.0.0.1:5353
    log
    errors
}
{{- end }}
. {
    template IN ANY . {
        rcode NXDOMAIN
    }
    log
    errors
}
{{- end }}
`

const dnsmasqTemplate = `# Enforcement back-end: resolves names CoreDNS already approved and records every answered
# address in the matching nftables set.
port=5353
listen-address=127.0.0.1
bind-interfaces
no-resolv
no-hosts
no-poll
server=127.0.0.11
{{- range $group := .Groups }}
{{- range $domain := $group.Domains }}
nftset=/{{ $domain }}/4#inet#hole#{{ $group.Name }}
{{- end }}
{{- end }}
`

const nftablesTemplate = `#!/usr/sbin/nft -f
# Re-creating the table makes this ruleset idempotent on container restart.
table inet hole
delete table inet hole

table inet hole {
{{- range .Groups }}
{{- if .Domains }}
    set {{ .Name }} {
        type ipv4_addr
    }
{{- end }}
{{- if .Statics }}
    set {{ .Name }}_static {
        type ipv4_addr
        flags interval
        # auto-merge collapses an address already covered by a CIDR in the same set; without it
        # nft rejects the whole ruleset with "conflicting intervals" and the gateway never starts.
        auto-merge
        elements = { {{ join .Statics ", " }} }
    }
{{- end }}
{{- end }}

    chain forward {
        type filter hook forward priority filter; policy {{ if .Unrestricted }}accept{{ else }}drop{{ end }};
{{- if not .Unrestricted }}
        meta nfproto ipv6 drop
        ct state established,related accept
{{- if .HostGatewayAll }}
        ip daddr {HOST_GATEWAY_IP} accept
{{- else if .HostGatewayPorts }}
        ip daddr {HOST_GATEWAY_IP} tcp dport { {{ ports .HostGatewayPorts }} } accept
        ip daddr {HOST_GATEWAY_IP} udp dport { {{ ports .HostGatewayPorts }} } accept
{{- end }}
{{- range .Groups }}
{{- if .Domains }}
        ip daddr @{{ .Name }} tcp dport { {{ ports .Ports }} } accept
        ip daddr @{{ .Name }} udp dport { {{ ports .Ports }} } accept
{{- end }}
{{- if .Statics }}
        ip daddr @{{ .Name }}_static tcp dport { {{ ports .Ports }} } accept
        ip daddr @{{ .Name }}_static udp dport { {{ ports .Ports }} } accept
{{- end }}
{{- end }}
        counter limit rate 10/minute log prefix "hole-denied " level info
{{- end }}
    }

    chain input {
        type filter hook input priority filter; policy accept;
        iifname "{SANDBOX_IF}" udp dport 53 accept
        iifname "{SANDBOX_IF}" tcp dport 53 accept
        iifname "{SANDBOX_IF}" ct state established,related accept
        iifname "{SANDBOX_IF}" drop
    }

    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "{INTERNET_IF}" masquerade
    }
}
`

var templateFuncs = template.FuncMap{
	"join": strings.Join,
	"ports": func(ports []int) string {
		parts := make([]string, 0, len(ports))
		for _, port := range ports {
			parts = append(parts, fmt.Sprint(port))
		}
		return strings.Join(parts, ", ")
	},
}

var (
	corefileTmpl = template.Must(template.New("Corefile").Funcs(templateFuncs).Parse(corefileTemplate))
	dnsmasqTmpl  = template.Must(template.New("dnsmasq.conf").Funcs(templateFuncs).Parse(dnsmasqTemplate))
	nftablesTmpl = template.Must(template.New("nftables.rules").Funcs(templateFuncs).Parse(nftablesTemplate))
)

// Generate renders the three gateway configuration files from a policy.
func (p Policy) Generate() (Artifacts, error) {
	corefileData := struct {
		HealthZone   string
		HostGateway  []HostGatewayDomain
		PolicyRegex  string
		Unrestricted bool
	}{
		HealthZone:   HealthZone,
		HostGateway:  p.HostGateway,
		PolicyRegex:  exprStringLiteral(p.PolicyRegex()),
		Unrestricted: p.Unrestricted,
	}

	var corefile strings.Builder
	if err := corefileTmpl.Execute(&corefile, corefileData); err != nil {
		return Artifacts{}, fmt.Errorf("generate Corefile: %w", err)
	}

	groups := p.Groups
	if p.Unrestricted {
		groups = nil
	}

	var dnsmasq strings.Builder
	if err := dnsmasqTmpl.Execute(&dnsmasq, struct{ Groups []Group }{Groups: groups}); err != nil {
		return Artifacts{}, fmt.Errorf("generate dnsmasq.conf: %w", err)
	}

	hostGatewayAll := false
	var hostGatewayPorts []int
	for _, entry := range p.HostGateway {
		if len(entry.Ports) == 0 {
			hostGatewayAll = true
			continue
		}
		hostGatewayPorts = normalizePorts(append(hostGatewayPorts, entry.Ports...))
	}

	nftData := struct {
		Groups           []Group
		Unrestricted     bool
		HostGatewayAll   bool
		HostGatewayPorts []int
	}{
		Groups:           groups,
		Unrestricted:     p.Unrestricted,
		HostGatewayAll:   hostGatewayAll,
		HostGatewayPorts: hostGatewayPorts,
	}
	var nftables strings.Builder
	if err := nftablesTmpl.Execute(&nftables, nftData); err != nil {
		return Artifacts{}, fmt.Errorf("generate nftables rules: %w", err)
	}

	return Artifacts{
		Corefile:     corefile.String(),
		DnsmasqConf:  dnsmasq.String(),
		NftablesRule: nftables.String(),
	}, nil
}

// exprStringLiteral prepares a regex for embedding in a CoreDNS `view` expression. The
// expr language processes escape sequences inside its string literals, so a regex
// backslash has to be doubled or CoreDNS refuses the config with "invalid char escape".
func exprStringLiteral(regex string) string {
	return strings.ReplaceAll(regex, `\`, `\\`)
}

// PolicyRegex builds the CoreDNS view expression: exact names match themselves, wildcard
// entries match strictly one-or-more extra labels (so `*.example.com` does not cover the
// apex). Returns "" when nothing is allowed, in which case no allowed view is emitted.
func (p Policy) PolicyRegex() string {
	alternatives := make([]string, 0, len(p.Exact)+len(p.Wildcards))
	for _, name := range p.Exact {
		alternatives = append(alternatives, regexp.QuoteMeta(name))
	}
	for _, name := range p.Wildcards {
		alternatives = append(alternatives, `([^.]+\.)+`+regexp.QuoteMeta(name))
	}
	if len(alternatives) == 0 {
		return ""
	}
	return `(?i)^(?:` + strings.Join(alternatives, "|") + `)\.?$`
}
