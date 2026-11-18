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
<domain>:<port>[,<port>...]                  network.hostGatewayDomains (ports mandatory, zone-wide)

host  := exact domain     example.com
       | wildcard domain  *.example.com      (subdomains only, not the apex)
       | IPv4 address     10.0.0.5
       | IPv4 CIDR        10.0.0.0/24
ports := 1-65535, omitted → 443,80
```

Ports apply to TCP and UDP alike. Entries come in two **layers**: *base* (settings and agents'
`allow.txt`, fixed at start) and *runtime* (added by [`hole modify`](#runtime-changes-hole-modify)).
Entries for the same host merge their ports within a layer, and base hosts are then grouped by
**identical port set** — one group per unique set. Each group's nftables set is named by content,
`b_<hash>` (first 10 hex characters of SHA-256 over layer and port list), so adding an unrelated
entry never renames a set and the addresses recorded in it survive. A runtime domain entry always
gets a set of its own, `r_<hash>` over layer, port list and host: dnsmasq only ever *adds*
addresses, so removing one entry of a shared set would leave its addresses reachable through the set
that stays, while its own set is simply deleted. For the same reason base and runtime entries never
share a set — runtime `github.com:80` next to base `github.com:22,443` is a separate set. Literal
IP/CIDR entries go into a static interval set per group, `<b|r>_<hash>_static`, hashed over the
elements too, so a changed address list is a new set rather than a flushed one.

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
enforcement would need a DNAT address per entry, which was considered and rejected (see #34).

`hostGatewayDomains` matching is **zone-wide, and inverted relative to `network.allow`**: the entry
grammar rejects `*.` (`domainPattern` has no `*`), yet every name *under* the entry resolves to the
host gateway anyway. Two CoreDNS behaviors compose into that:

- each entry renders a **server block** (`mydb.local:53 { … }`), and CoreDNS routes a query to the
  block with the longest matching zone suffix — so `db.mydb.local` is handled there, never reaching
  the `view allowed` block or the catch-all NXDOMAIN;
- inside the block the `template` plugin is given no `match`, and its default is `.*` ("specifying
  no regex matches everything"), so every qname in the zone is answered with `{{ .Name }} 60 IN A
  {HOST_GATEWAY_IP}`.

`BuildPolicy` never copies these names into `Exact`/`Wildcards`, so the zone block is the only
reason they resolve at all, and they emit no `nftset` lines — the firewall side is name-blind, so a
resolving subdomain reaches nothing the port union did not already open.

What it does change is **shadowing**: an entry naming a real domain captures that whole zone, so
`example.com:8080` makes `api.example.com` answer with the host gateway address even when
`api.example.com` is in `network.allow` — the specific zone block wins over `.`. Documented in the
README as "use names you control"; enforcing it (rejecting publicly resolvable suffixes) would need
a PSL copy in the binary and is not attempted.

The address itself is read from the gateway container's **`/etc/hosts`**, where the runtime writes
the entry compose asks for (`extra_hosts: host.internal:host-gateway`), plus Podman's built-in
`host.containers.internal`. Not through `getent hosts`: that goes through nsswitch, which answers
AF_INET6 first, and a runtime injecting both an IPv4 and an IPv6 entry for the name (OrbStack does)
then leaves an IPv4-only filter with nothing — the bug that made the whole feature resolve to
`127.0.0.1` there. When no usable IPv4 address is found and the generated Corefile carries the
`{HOST_GATEWAY_IP}` placeholder, the entrypoint aborts; the Corefile is the signal, so the check
cannot drift out of sync with the policy. `TestGatewayEntrypointResolvesHostGatewayFromHostsFile`
(`assets/assets_test.go`) pins the lookup, and two integration tests
(`internal/sandbox/gateway_integration_test.go`) run the real entrypoint against a fabricated
`/etc/hosts` — one dual-stack, one loopback-only.

The `extra_hosts` entry lands in the **gateway** container only. The agent container gets no
runtime-injected name for the host, so a `host.internal:<port>` entry is resolved by the gateway's
CoreDNS zone block like any other name — which is why the README documents `host.internal` as *the*
name for host services instead of asking users to invent one.

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
| `dnsmasq.conf` | one `nftset=/<key>/4#inet#hole#<set>[,…]` line per domain key (see below), upstream `127.0.0.11` |
| `nftables.rules` | one `inet hole` table: a dynamic set per group, a static interval set for IP/CIDR entries, forward chain with policy `drop`, masquerade on the internet interface, IPv6 dropped, rate-limited logging of denials |

`Generate` also renders `nftables.apply`, the same ruleset in **in-place mode** for
[`hole modify`](#runtime-changes-hole-modify): instead of `delete table` + full table it starts
with `flush chain inet hole forward`, then declares the sets (nft creates missing ones and leaves
existing ones and their elements alone) and re-declares the `forward` chain with its rules. One
`nft -f` is one transaction, so there is no moment without rules. `input` and `postrouting` never
change at runtime and are not in it; no set is flushed or destroyed there (flushing a set that does
not exist yet fails the whole file, and `destroy` needs kernel 6.3) — unused sets are deleted by the
apply script afterwards. Both modes are golden-tested.

**dnsmasq rules.** dnsmasq uses exactly one `nftset=` line per query — the longest matching
suffix — and an exact `x` and a wildcard `*.x` share the key `x`. The generator therefore emits one
line per key `k` carrying the sets of exact entries for `k` and of wildcards whose host is `k` or a
parent of `k`, so every name dnsmasq routes to line `k` feeds every set that may permit it
(`nftsetRules`, `gateway.go`). Otherwise a runtime `api.github.com:8443` under a base
`*.github.com` would lose the wildcard's ports for that name, and a tied exact/wildcard pair would
produce two lines of which dnsmasq ignores one. The remaining over-grant is listed under
[accepted limitations](#accepted-limitations).

**Generation marker.** The health zone also answers `TXT` with the Corefile's generation (first 12
hex characters of SHA-256 over the Corefile rendered without the marker), which is how a runtime
apply confirms that CoreDNS reloaded. The healthcheck (`A`) is unaffected.

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
  with the drop in force, so a gateway that cannot configure itself leaves the sandbox with no route
  rather than an unfiltered one. Host gateway resolution is an explicit `exit 1` rather than a
  `set -euo pipefail` casualty — an unresolved address used to be substituted with `127.0.0.1`
  instead, which started the sandbox with the feature silently pointing at the container itself.

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

## Hosts that filter bridged packets (br_netfilter)

The gateway-as-router design assumes a same-bridge hop is pure L2 switching that host iptables
never sees. That assumption breaks when `br_netfilter` is active with
`net.bridge.bridge-nf-call-iptables=1`: bridged frames are then pushed through the host's
`FORWARD` chain, where Docker's rules for `--internal` networks drop anything whose IP
destination lies outside the sandbox subnet — which is exactly what agent→gateway egress looks
like. The SYN dies on the bridge, the gateway never sees it, and because DNS and the
healthcheck have in-subnet destinations, the sandbox starts "healthy" and then every connection
hangs. Most hosts are unaffected (Docker ≥27.3 only enables the sysctl when it needs it); k3s,
kubeadm, some WSL setups and leftover `/etc/sysctl.d` entries set it.

The repair is one iptables rule at the top of `DOCKER-USER` — the chain Docker evaluates before
every rule it writes itself and reserves for users:

```
-i br-<sandbox> -o br-<sandbox> -m physdev --physdev-is-bridged -j ACCEPT
```

It can only match frames that are *switched* with the sandbox bridge as both in- and out-device,
i.e. traffic that never leaves that bridge and can only terminate at another sandbox container.
Routed traffic always has a different out-device, so Docker's isolation and the `--internal`
boundary are untouched. This is a strictly narrower intervention than turning the sysctl off,
which is per-netns-global and breaks host-mode Kubernetes.

Mechanics (`internal/sandbox/bridgefilter.go`, `assets/gateway/hole-bridge-netfilter`):

- The CLI is unprivileged and, on VM-based engines, on the wrong machine — so the work runs in
  a one-shot helper container in the **daemon's** network namespace
  (`docker run --rm --network host --cap-add NET_ADMIN`, `engine.RunHostHelper`), using the
  gateway image, which provably exists right after the staged `compose up gateway`. The helper
  runs there and then, before the agent starts — no window in which the agent runs unrepaired.
  That helper round-trip is a deliberate per-start cost even on healthy hosts: the sysctl must
  be read in the daemon's namespace, and no cheaper proxy for it is trustworthy (a local
  `/proc` read is wrong under a VM-based or remote daemon, and `docker info`'s
  bridge-nf warning is cached from daemon start).
- The helper first sweeps every stale Hole rule, then reads the sysctl in that namespace;
  absent or `0` means exit without installing anything, keeping healthy hosts' firewalls
  untouched — the sweep still runs, so rules from an affected era are collected once the host
  is healthy again. Otherwise it picks the iptables backend by where the `DOCKER-USER` chain
  actually exists (`iptables-nft` preferred over `iptables-legacy` — the daemon's rules are
  invisible through the other backend) and installs this sandbox's rule idempotently. No chain
  in either backend (native nftables daemon, `iptables: false`) exits 3 and **fails the
  start** with guidance — the sandbox would come up with dead egress. Every iptables call
  waits for the xtables lock at most 5 seconds and the helper container itself is bounded by
  `engine.RunHostHelper`'s 60-second timeout, so a held lock degrades into the
  warn-and-continue path instead of hanging the start.
- A rule is stale when its `br-*` interface no longer exists in the daemon's namespace — the
  bridge is global truth on a shared daemon, unlike the per-user instance registry, so the
  sweep can never delete the rule of another user's running sandbox. The instance's own
  comment is always kept, and a helper without a working `ip` skips the sweep rather than
  guessing.
- The rule is comment-tagged with the instance name. Teardown removes it by comment
  (best-effort, warns with the manual removal command if the gateway image is already gone —
  the image tag, the backend and the physdev/no-physdev variant are recorded in
  `state.Instance`, so the printed command names the rule that actually exists even after an
  upgrade). A leftover rule is inert — its bridge died with the network — and the next
  start's sweep collects it.
- `network.bridgeNetfilterFix: "off"` disables all of it; docker engine only, decided by the
  runtime's `--version` banner rather than the binary name, so a `$HOLE_RUNTIME` path or
  wrapper around real docker still gets the fix (netavark writes different rules — revisit
  with podman support).

Accepted residual risk: a firewalld reload or an admin flush of `DOCKER-USER` mid-run silently
re-breaks egress until the next start reinstalls the rule — the same class of event that flushes
Docker's own rules.

## Unrestricted mode (`-u`)

CoreDNS forwards everything unconditionally, dnsmasq drops its nftset lines, and the forward chain
policy becomes `accept`. Masquerading stays, since the sandbox still has no route of its own.

## Runtime changes (`hole modify`)

`hole modify` changes `network.allow` and `network.hostGatewayDomains` of a **running** sandbox
(`internal/sandbox/modify.go`). There is still one generator: the CLI re-renders the complete
configuration from the base entries recorded in the instance state plus the runtime entries, and the
running gateway applies it **in place** — the container is never recreated, because a new container
would lose its conntrack/NAT state (every open connection, including the agent's in-flight API
request) and every address dnsmasq recorded. Every apply sends the complete desired configuration,
so any apply also repairs an earlier partial one.

**Transport.** `docker exec -i <instance>-gateway-1 hole-gateway-apply` with a tar stream on stdin
(`engine.ExecInput`). The host-side files are single-file bind mounts, which do not see an atomic
rename and behave differently on VM-based engines; `exec` works the same everywhere. No listener,
file or channel the agent could reach is added, and `exec` reuses the gateway's existing
`NET_ADMIN`.

**Payload.** `Corefile`, `dnsmasq.conf` and the start-mode `nftables.rules` of the new
configuration; `open.nft`, the in-place ruleset for base ∪ previous ∪ new runtime entries;
`final.nft`, the in-place ruleset for base ∪ new runtime entries; and
`previous/{Corefile,dnsmasq.conf,apply.nft}` for rollback.

**Order** (`assets/gateway/hole-gateway-apply`, fail-closed). Under `flock /run/hole-apply.lock`:
validate first (`dnsmasq --test`, `nft -c -f` of both rulesets — exit 11, nothing changed), and
refuse a host gateway zone without a usable address (exit 10). Then `open.nft` → restart dnsmasq if
its file changed → reload CoreDNS if the Corefile changed → `final.nft` → delete the `b_*`/`r_*`
sets `final.nft` no longer declares. A name never resolves before its addresses can be recorded and
permitted, and a removed name stops resolving before its rules go — also for a mixed add + remove.
A failure in those steps re-runs them with `previous/` (exit 12); if even that fails, exit 13 — the
gateway may be left in the "open" state, and re-running any `modify` re-converges it.
`TestGatewayApplyOrder` (`assets/assets_test.go`) pins the order.

- **dnsmasq** cannot reload `nftset=` lines, so it is restarted. The entrypoint runs it under a
  supervising loop that records each PID in `/run/hole-dnsmasq.pid`; the apply kills that PID,
  waits until it is gone and the new dnsmasq answers `version.bind CH TXT` on port 5353. The kernel
  sets survive the restart. A side benefit: a crashed dnsmasq now comes back.
- **CoreDNS** is reloaded with `SIGUSR1` to PID 1, which is graceful: an invalid Corefile keeps the
  old configuration. The apply polls the health zone's `TXT` answer until it carries the new
  [generation](#generated-per-run-artifacts); a reload that never gets there is a failure.

**Lifetime.** After a successful apply the script stores the unsubstituted `Corefile`,
`dnsmasq.conf` and `nftables.rules` in the container's own writable layer —
`/var/lib/hole/runtime.<random>/`, then an atomic symlink swap of `/var/lib/hole/runtime`. When
Docker restarts the gateway container (`restart: on-failure`), the entrypoint reads that directory
instead of `/etc/hole/` when all three files are present. Teardown removes the container and its
layer, so a new `hole start` always begins with the base list.

**Host side.** The base entries (with their sources) and the runtime entries live in
`state.Instance` (see [architecture](architecture.md#runtime-changes-and-the-instance-lock)), so listing, removing and
re-rendering never depend on settings files or agents' `allow.txt` changing mid-run. Validation
before anything is applied: entries parse, `docker.io`/`*.docker.io` are refused (the
[capability token](#docker-hub-is-a-capability-token) is a start-time decision), `network.allow`
under `-u` is refused, and host gateway shadowing is refused both ways (a runtime zone over an
effective allow entry, a runtime allow entry under any zone — `HostGatewayConflicts`). The gateway
must be `healthy`: an apply racing a restarting entrypoint would be undone by its `delete table`.

**Blocked names** (`list-blocked`) come from the gateway's CoreDNS log (`docker logs -t`; `log` is
always on): a name is blocked when it has an `NXDOMAIN` line **and** the current policy does not
match it (`Policy.Matches`, which mirrors the Corefile regex), so a stale NXDOMAIN from before a
runtime change and an upstream NXDOMAIN of an allowed name are both left out. Names are lowercased,
deduplicated, stripped of one trailing dot; reverse zones, the health zone and anything that is not
a valid domain name (which also drops control characters and terminal escapes) are dropped. The
CoreDNS line parser is shared with the `-n` dump (`parseCoreDNSQuery`).

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
- **Names sharing a dnsmasq key share sets.** dnsmasq routes every name to one `nftset=` line,
  and that line feeds all sets that may permit *some* name routed to it. With `x` and `*.x` both
  allowed (on different port sets), the apex addresses of `x` land in the set of `*.x` and vice
  versa. With `*.example.com:443` and `api.example.com:8080`, `foo.api.example.com` (permitted by
  the wildcard) is routed to the `api.example.com` line and lands in the `:8080` set too. Without a
  wildcard there is no leak: `example.com:22` next to `api.example.com:443` stay apart. This is a
  strict subset of the shared-address limitation above.
- **A removed runtime entry can leave addresses in a base set.** Its own set is deleted, but when
  it shared a dnsmasq key with a base entry (runtime `github.com:443` next to base
  `*.github.com:443`), the addresses its names resolved to were also recorded in the base set and
  stay reachable on the base entry's ports until the sandbox ends. dnsmasq cannot tell apex from
  subdomain on one key, so the generator cannot avoid it.
- **CNAME chains** work without allowing the CDN vendor: dnsmasq records every address from the
  chased answer.
- **UDP** is stateful via conntrack, so QUIC to an allowed `host:443` works.
