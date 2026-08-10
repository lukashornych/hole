# Networking

All sandbox egress flows through one container: the `gateway`, which is simultaneously the
sandbox's DNS server, router and firewall. Filtering happens at L3/L4 rather than in an HTTP
proxy, so **every protocol and port** is covered and no tool inside the sandbox needs proxy
configuration.

```
agent ──(default route + DNS)──▶ gateway ──(masquerade)──▶ internet network
                                  ├─ CoreDNS :53    DNS policy: answers only allowed names,
                                  │                 NXDOMAIN for everything else
                                  ├─ dnsmasq :5353  resolves approved names and records every
                                  │                 answered address in an nftables set
                                  └─ nftables       forward chain, default drop
```

Two independent layers:

- **DNS**: a name that is not allowed does not resolve at all — a fast, clean `NXDOMAIN` instead
  of a timeout.
- **Firewall**: an address:port is only reachable if the sandbox's own resolver handed that
  address out for an allowed entry, or it is a literal IP/CIDR entry. Hardcoded-IP connections,
  third-party resolvers (`dig @8.8.8.8`) and DNS-over-TLS are therefore all denied.

**Why two resolvers in one container:** dnsmasq is the only mainstream resolver that populates
nftables sets natively (`--nftset`), but its domain matching is suffix-wide, which would make
`example.com` also match `evil.example.com`. CoreDNS sits in front as the policy gate: its `view`
plugin admits only qnames matching a generated regex (exact names plus explicit `*.` wildcards),
and a catch-all block answers NXDOMAIN for the rest. Since only approved names ever reach
dnsmasq, its broader matching is harmless — it only maps answered addresses to port sets. Both
run in the same container because nftset writes go to the kernel of the network namespace dnsmasq
runs in: resolver and firewall must share a namespace.

## The allow-list model

`internal/network` parses every source — each enabled agent's `allow.txt`, the user's
`network.allow`, and `network.hostGatewayDomains` — into one policy. Entry grammar:

```
<host>[:<port>[,<port>...]]                  network.allow, agent allow.txt
<domain>:<port>[,<port>...]                  network.hostGatewayDomains (ports mandatory)

host  := exact domain     example.com
       | wildcard domain  *.example.com      (subdomains only, not the apex)
       | IPv4 address     10.0.0.5
       | IPv4 CIDR        10.0.0.0/24
ports := 1-65535, omitted → 443,80
```

Ports apply to TCP and UDP alike. Entries for the same host merge their ports, and hosts are then
grouped by **identical port set** — one group per unique set, named `g0`, `g1`, … in a
deterministic order.

`hostGatewayDomains` entries merge the same way, and they have to: each one renders a CoreDNS
server block, and CoreDNS refuses a Corefile that defines the same zone twice — so two entries for
one domain (easily produced by the global and the project file naming different ports) would kill
the gateway and with it every start.

Their port list is **mandatory**, and the firewall side of them is coarser than the Corefile side:
every entry resolves to the same host gateway address, and nftables matches address plus port, so
one flat union of all configured ports is emitted (`Generate`, `gateway.go`). The sandbox can reach
the host gateway IP on that union directly, without DNS — the names choose what resolves, not what
the firewall permits. Requiring ports is what bounds the union; a port-less entry used to emit a
bare `ip daddr {HOST_GATEWAY_IP} accept`, i.e. every service on the developer's machine. Per-name
enforcement would need a DNAT address per entry, which was considered and rejected
([analysis](../analysis/host-gateway-mandatory-ports-plan.md)).

A malformed entry is fatal. A wrong allow list makes the sandbox unsafe or broken, which is not a
skippable warning.

### Docker Hub is a capability token

One host does more than open itself. `network.DockerHubToken` (`docker.io`) also decides whether a
Docker-in-Docker sandbox gets the pull-through image cache: `Policy.AllowsDockerHub()` is true when
the resolved policy carries that host as an exact or wildcard entry, or when filtering is off (`-u`),
and only then does `start.go` attach `hole-registry` to the sandbox network — see
[configuration](configuration.md#docker-in-docker).

The token exists because the mirror is reached over the *unfiltered* sandbox network, so the cache is
a Hub channel the gateway never sees. Gating it on an explicit entry is what keeps "the sandbox can
reach Docker Hub" a decision recorded in the allow list rather than a side effect of
`container.docker`.

Two consequences follow from the two accepted spellings, and both are documented in the README
because users hit them:

- `"docker.io"` alone opens the apex (which no pull contacts) and the cache. If the cache fails to
  start there is no fallback: Hub pulls simply fail.
- `"*.docker.io"` also opens `registry-1.docker.io` and `auth.docker.io`, i.e. most of a direct-pull
  path through the gateway. Blob fetches redirect off `docker.io` — to `*.cloudflare.docker.com` as
  of writing — so a direct-pull allow list needs that domain too. That last part is documented from
  Hub's published behavior, not verified here against a live pull; a `-n` dump names the actual host
  if one is denied.

## Generated per-run artifacts

Three files are rendered from the policy into the run directory and bind-mounted read-only. All
of them are golden-tested (`internal/network/testdata/`), so any change to the output is visible
in review.

| File | Role |
|---|---|
| `Corefile` | policy front-end: a `view`-guarded root block forwarding to dnsmasq, `hostGatewayDomains` blocks answering the host gateway address, a catch-all NXDOMAIN block, a health zone, and `log` on the query blocks (which powers `-n`) |
| `dnsmasq.conf` | `nftset=/<domain>/4#inet#hole#<set>` per domain, upstream `127.0.0.11` |
| `nftables.rules` | one `inet hole` table: a dynamic set per group, a static interval set for IP/CIDR entries, forward chain with policy `drop`, masquerade on the internet interface, IPv6 dropped, rate-limited logging of denials |

Placeholders `{HOST_GATEWAY_IP}`, `{SANDBOX_IF}` and `{INTERNET_IF}` are substituted by the
gateway entrypoint.

Two details that are easy to get wrong:

- The CoreDNS `view` expression is an **expr string literal**, and expr processes escape
  sequences inside it. A regex backslash must therefore be doubled, or CoreDNS rejects the config
  with "invalid char escape".
- Interfaces are identified **by address**, matched against the sandbox subnet passed in as
  `HOLE_SANDBOX_SUBNET`. Compose does not guarantee eth0/eth1 ordering across two networks.
- The static set is declared `auto-merge`. An interval set rejects overlapping elements — an
  allow list holding both `10.0.0.0/24:443` and `10.0.0.5:443` fails `nft -f` with "conflicting
  intervals", which under the entrypoint's `set -euo pipefail` means no gateway at all.
  `auto-merge` collapses the pair into the union, which is what the entries mean.

## Gateway startup is fail-closed

The gateway container is created with `net.ipv4.ip_forward=1` (compose `sysctls`), so it routes from
the moment it starts — before the entrypoint has read a single generated file. Forwarding cannot be
deferred to the entrypoint instead: `/proc/sys` is read-only in an unprivileged container, so
`sysctl -w` there fails with EROFS. The entrypoint's **first** action is therefore to install an
`inet hole` table whose forward chain is `policy drop`, which the generated ruleset then replaces in
a single `nft -f` transaction — the swap opens no window. Two consequences worth keeping:

- The interval between container start and the real policy is a drop, not an accept. That matters
  beyond first start, because `restart: on-failure` can bring the gateway back mid-run while the
  agent is live.
- Every check below the drop (`nftset` support, host gateway resolution, interface discovery) aborts
  under `set -euo pipefail` with the drop in force, so a gateway that cannot configure itself leaves
  the sandbox with no route rather than an unfiltered one.

`TestGatewayEntrypointDropsForwardingFirst` (`assets/assets_test.go`) pins the order: the drop has
to precede every other startup step and the generated ruleset.

## The gateway healthcheck

The agent service waits on `service_healthy`, so the probe decides whether a sandbox starts at
all. It resolves a dedicated zone (`health.hole.internal`) rather than any user-configured domain,
and two things about it are load-bearing:

- The zone answers **AAAA as an empty NOERROR** as well as A. A zone that answers only A SERVFAILs
  the AAAA query, and a bare `nslookup <name>` asks both — so the probe could never pass.
- The probe is `dig` with an **explicit record type**, matching the answer with `grep -qx`. `dig`
  exits 0 on SERVFAIL, so the exit status alone proves nothing; and the match is anchored to the
  whole line because `dig` prints `;; communications error to 127.0.0.1#53` on *stdout* when
  nothing is listening — an unanchored search for the address matches that error text.

When a service does not come up, `internal/sandbox` prints the last failed probe output and the
tail of the container log before teardown removes the container. Compose itself reports only
`dependency failed to start: container … is unhealthy`.

## Base image constraint: dnsmasq needs nftset

The gateway is built on `ubuntu:24.04` rather than Alpine for one hard reason: **Alpine's dnsmasq
package is compiled with `no-nftset`** (visible in `dnsmasq --version`), so the firewall sets
would never be populated and nothing would be reachable. Debian and Ubuntu builds enable it. The
entrypoint re-checks the capability at startup and exits with a clear error rather than running
with a permanently empty rule set. That check captures `dnsmasq --version` into a variable instead
of piping it into `grep -q`: under `pipefail`, `grep -q` exiting on the first match can leave
dnsmasq killed by SIGPIPE, failing the check against a build that does support nftset.

If a future base image drops the feature, the documented fallback is dnsmasq `--ipset` with an
iptables variant of the generated ruleset.

Since the agent image is Ubuntu 24.04 too, this shares layers instead of adding a base image.

The apt-installed pieces (`dnsmasq-base`, `nftables`) come from signed repositories, but CoreDNS is
a release tarball from GitHub, so the Dockerfile pins its version *and* a per-architecture sha256
and verifies the download before extracting it. The policy engine is not installed on the strength
of TLS alone — see
[build & release](build-and-release.md#pinned-third-party-artifacts) for how to refresh the pin.

## Route injection

The sandbox network is `internal: true`, so there is no route off it except through the gateway.
The agent and DinD containers replace their default route with the gateway address at startup
(`ip route replace default via $HOLE_GATEWAY_IP`), which needs `cap_add: NET_ADMIN` on the agent
and `iproute2` in the image.

`NET_ADMIN` on the agent is safe: all filtering happens on the gateway, so rewriting routes inside
the agent can only break its own connectivity, never widen it.

Proxy environment variables (`HTTP_PROXY` and friends) are **not** set on any service. Removing
them is what makes non-proxy-aware tools work.

## Unrestricted mode (`-u`)

CoreDNS forwards everything unconditionally, dnsmasq drops its nftset lines, and the forward chain
policy becomes `accept`. Masquerading stays, since the sandbox still has no route of its own.

## Subnet allocation

`network.subnetPool` (default `10.222.0.0/16`, minimum `/23`) is Hole's own pool. Each instance
takes two `/24`s. This replaces the 1.x probe-network trick, which created a throwaway network to
see what Docker's IPAM picked — it leaked, raced, and burned a Docker default-pool `/16` per
network, exhausting the pool after ~14 sandboxes.

Allocation lists every existing network's subnets in one pass, then:

- attempt 0 takes the lowest free `/24`s, which keeps single-sandbox subnets predictable;
- retries pick at **random**, because strict first-fit makes concurrent starts stampede the same
  candidate (12 simultaneous first-fit starts exhaust the retry budget; with jitter, 20 succeed
  with 20 unique subnets);
- **overlap**, not equality, is the test — an existing supernet covering the pool correctly blocks
  every candidate;
- `network create` is atomic in the runtime, so a concurrent start that picked the same subnet
  fails there and retries rather than producing an overlapping network.

Capacity from a `/16` is ~127 concurrent sandboxes. The exhaustion error states free/total
capacity so "too small" is distinguishable from "full".

## Network access dump (`-n`)

The gateway container's CoreDNS query log is parsed on teardown: `NXDOMAIN` answers become
`DENIED <name>`, everything else `ALLOWED <name>`. Output goes to
`~/.hole/logs/<project>/network-access-<agent>-<instance>.log` — under `~/.hole`, never the
project's own `.hole/logs`, which is bind-mounted read-write with the host UID and so could be
replaced with a symlink to redirect this host-side write (`internal/sandbox/dump.go`).

Documented limitation: direct-IP attempts blocked by the firewall never produce a DNS query, so
they do not appear in the dump. The nftables counter records them for debugging
(`nft list ruleset` from a debug shell).

## Accepted limitations

- **Shared/rotating addresses.** Once an allowed name resolves, that address:port stays reachable
  for the sandbox's lifetime — nftset entries do not expire, because expiry would break
  long-lived connections on re-resolution. An agent could therefore reach a *different* site on
  the same address, which is common with CDNs. Future hardening is SNI verification on TLS flows.
- **dnsmasq's suffix matching across port groups.** If `example.com:22` and `api.example.com:443`
  are both allowed, dnsmasq's suffix match adds `api.example.com`'s addresses to the `:22` group
  as well. This is a strict subset of the shared-address limitation above.
- **CNAME chains** work without allowing the CDN vendor: dnsmasq records every address from the
  chased answer.
- **UDP** is stateful via conntrack, so QUIC to an allowed `host:443` works.
