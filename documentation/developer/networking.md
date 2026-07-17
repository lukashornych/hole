# Networking

All sandbox egress flows through two infrastructure containers: a filtering HTTP/HTTPS proxy
(tinyproxy) and a DNS server (CoreDNS). The agent container itself has no internet route — it
sits on the internal `sandbox` network (see [architecture](architecture.md#container-architecture)).

## Proxy (tinyproxy)

- Image: Alpine + tinyproxy (`proxy/Dockerfile`), listening on port 8888, healthchecked with
  `nc -z localhost 8888`.
- The agent (and DinD sidecar) route traffic to it via `HTTP_PROXY`/`HTTPS_PROXY`/lowercase
  variants set in `agents/docker-compose.yml`. `NO_PROXY=localhost,127.0.0.1` (extended with
  `docker` when DinD is enabled) keeps local traffic direct.
- Base configs:
  - `proxy/tinyproxy.conf` — filtering enabled: `Filter /etc/tinyproxy/allowed-domains.txt`,
    `FilterDefaultDeny Yes`, `FilterExtended On`.
  - `proxy/tinyproxy-unrestricted.conf` — used with `-u/--unrestricted-network`;
    `FilterDefaultDeny No`, i.e. everything allowed.
- `LogLevel Connect` writes every allowed/refused connection to
  `/var/log/tinyproxy/tinyproxy.log`, which powers the network access dump (below).

### Generated per-run config

`generate_instance_compose()` never mounts the base config directly. It generates
`${HOLE_TMP_DIR}/tinyproxy.conf` by stripping all `ConnectPort` lines from the chosen base
config and appending the effective list:

- No `network.allowedPorts` in settings → defaults `ConnectPort 443` + `ConnectPort 80`.
- `network.allowedPorts: [...]` → one `ConnectPort` line per port (the merged global+project
  list **replaces** the defaults).
- `network.allowedPorts: []` → `ConnectPort 0`, disabling CONNECT entirely.

### Whitelist merging

The merged whitelist is written to `${HOLE_TMP_DIR}/tinyproxy-domain-whitelist.txt` and
bind-mounted over `/etc/tinyproxy/allowed-domains.txt`. Merge order:

1. `proxy/allowed-domains.txt` — the default shared base (intentionally empty)
2. `agents/<agent>/allowed-domains.txt` for **every enabled agent** (not just the startup agent)
3. `network.domainWhitelist` from merged settings — plain domain names; dots are escaped
   (`.` → `\.`) because tinyproxy filter entries are regexes
4. `network.hostGatewayDomains` — auto-whitelisted so the proxy allows traffic to them

Agent whitelist files contain raw tinyproxy regex patterns (e.g. `.*\.anthropic\.com`);
user-supplied domains are literal names that Hole escapes itself.

### Network access dump (`-n` / `--dump-network-access`)

During teardown phase 1, the proxy container is stopped gracefully (so tinyproxy flushes its
stdio buffers), its log is copied out with `docker cp`, and distinct domains are extracted:

- `Established connection to host "..."` → `ALLOWED <host>`
- `Proxying refused on filtered url|domain "..."` → `DENIED <host>`

The sorted unique result is written to
`<project>/.hole/logs/network-access-<agent>-<instance_id>.log`. Useful for building a minimal
`network.domainWhitelist` for a project.

## DNS (CoreDNS)

- Image: Alpine + a pinned CoreDNS release binary (`dns/Dockerfile`, `COREDNS_VERSION` build arg).
- The DNS container joins **both** networks and gets a **fixed IP** on the sandbox network:
  the subnet base with last octet `.53` (e.g. `172.19.0.53`). This is why the sandbox network is
  pre-created with an explicit subnet — `ipv4_address` requires it. The proxy, agent, and DinD
  services all list `${dns_ip}` first and Docker's embedded DNS (`127.0.0.11`) as fallback.
- Default behavior (`dns/Corefile`): forward everything to `127.0.0.11`, i.e. normal Docker DNS.

### Host gateway domains

`network.hostGatewayDomains` lets the agent reach services running on the **host** under a
stable domain name:

1. `hole.sh` validates each entry is a plain domain name and warns about `localhost`
   (it is in `NO_PROXY`, so it would bypass the proxy and never reach the host).
2. A Corefile is generated in `HOLE_TMP_DIR` with one server block per domain answering
   `A → {HOST_GATEWAY_IP}` (and empty `AAAA`), plus the catch-all forward block.
3. `dns/entrypoint.sh` resolves `host.internal` (added via `extra_hosts: host-gateway` in
   `dns/docker-compose.yml`) at container start and substitutes the placeholder before
   exec-ing CoreDNS.
4. The proxy container gets matching `extra_hosts` entries so it can also resolve those domains,
   and the domains are appended to the proxy whitelist automatically.

Result: the agent resolves `mydb.local` → DNS container → host gateway IP, and the proxy allows
the CONNECT through.
