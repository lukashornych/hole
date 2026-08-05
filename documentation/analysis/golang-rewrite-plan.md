# Golang Rewrite — Implementation Plan

Companion to [golang-rewrite.md](golang-rewrite.md) (the requirements analysis). This plan turns
the analysis plus four previously separate feature plans into a concrete, phased implementation
roadmap. It is **self-contained**: the content of the former `.claude/plans/` documents
(`NETWORK-TRANSPARENT-GATEWAY-PLAN.md`, `PROFILES-PLAN.md`, `SHARED-AGENT-IMAGES-PLAN.md`,
`NETWORK-CLEANUP-FIX-PLAN.md`) is inlined here, adapted from their bash framing to the Go
rewrite; the originals can be deleted.

The network-cleanup design (pooled subnet allocator, startup GC, deterministic teardown) is
treated as **not implemented** — it ships as a new feature of the Go version (§6.8, §7.3–7.4),
designed against the current bash behavior (probe-network allocator, teardown gated on full
startup success).

## 1. Decisions locked (analysis + explicit answers)

| Decision | Choice |
|---|---|
| Docker orchestration | **Option A** — shell out to `docker compose` / `docker`; no Engine API SDK. Every runtime invocation confined to one package (`internal/engine`) |
| Compose files | Generated **entirely in Go** from typed structs (`yaml.v3` tags, only the ~20 fields Hole uses). The 5-file layering disappears — one generated compose file per run |
| Cleanup reliability | **Watchdog + GC hybrid**: a detached watchdog process (same binary, hidden subcommand) is the **sole runtime executor** of teardown — the CLI waits on it and relays its output, falling back to inline teardown only if the watchdog died; label-based GC on every `start`/`list` as backstop |
| DinD image cache | **Pull-through registry replaces the volume seed/sync mechanism** (flock dance deleted). Hole manages a long-running registry container automatically |
| Networking | **Transparent gateway** (§6) — built directly in Go; tinyproxy config generation is *not* ported first |
| Git workspaces | **git worktrees** (detected via `git rev-parse --git-common-dir`) |
| Distribution | Go best practice: **GoReleaser** static binaries on GitHub Releases + OS/arch-detecting `install.sh` + in-binary **self-update** with checksum verification (pattern used by flyctl, k9s, task) |
| Profiles / shared images | Full specifications in §10 and §11 |
| User-facing API | CLI commands, flags, `--` passthrough and `settings.json` format stay compatible, except the new features, the two deliberate bug fixes (§4), and the 2.0 settings-key removals with migration errors (§6.2) |

## 2. Tech stack

- **Go 1.25.x**, module `github.com/lukashornych/hole`, single static binary (CGO_ENABLED=0).
- **Runtime dependencies for the end user**: docker *or* podman with the compose plugin. Nothing
  else — `jq`, `jv`, `sha1sum`, `curl`/`wget`, `tar`, `flock` are all replaced by Go stdlib or
  Go libraries. `git` becomes an *optional* dependency (worktree detection; warn + skip if absent).
- **Go libraries** (few, mainstream, each replacing an external tool):
  - `gopkg.in/yaml.v3` — compose file generation (hand-rolled structs, per the analysis's own
    verification of the compose-go round-trip risk)
  - `github.com/santhosh-tekuri/jsonschema/v6` — settings validation (the library behind `jv`;
    schema semantics identical, including draft 2020-12 `unevaluatedProperties` needed by
    profiles, §10.2)
  - `github.com/creativeprojects/go-selfupdate` — `hole update` (GitHub release discovery,
    checksum verification, atomic binary replacement)
  - stdlib elsewhere: `log/slog` (logging), `encoding/json` (merging — replaces jq),
    `crypto/sha1` (project names, image hashes), `crypto/rand` (instance IDs — fixes the WSL
    `/dev/urandom` edge natively), `path/filepath` + `io/fs` glob (exclusion patterns),
    `os/exec` (engine calls), hand-rolled CLI parsing (§4)
- **Assets embedded via `go:embed`**: Dockerfiles, container entrypoints, agent plugin files
  (`command.json`, `allow.txt`, install scripts), gateway config templates, the JSON schema.
  Materialized into the per-run tmp dir when a build context is needed. **This retires the
  "add every new file to release.yml" packaging rule** — the binary is the package. The
  replacement rule: new runtime assets must be under an embedded directory (compile fails
  loudly if an `embed` directive misses, unlike the silent tarball omission today).
- **Testing**: `go test`; unit tests colocated per package; integration/e2e tests behind the
  `integration` build tag requiring a real Docker daemon (§13).

## 3. Repository layout

The Go code replaces the bash implementation in this repository (branch `go-rewrite`, PRs to `dev`):

```
cmd/hole/main.go            # thin entry: wire logging, dispatch to internal/cli
internal/cli/               # arg parsing (incl. agent:profile split, -- passthrough), help text
internal/config/            # settings load, schema validation, deep merge, profiles + extends,
                            #   agents.args no-dedup exception, canonical path pipeline entry
internal/hostenv/           # expand_env_vars port, tilde/relative resolution, host identity
                            #   (username/home/uid/gid, Linux-only uid rule), tmp dir under ~/.hole/tmp
internal/engine/            # THE single package that shells out to docker/podman:
                            #   runtime detection (HOLE_RUNTIME → docker → podman, compose check),
                            #   compose up/down, network/volume/image/container ops, attach, events.
                            #   No interfaces/abstraction — just all call sites in one place (per analysis §Recommendation)
internal/compose/           # typed compose model (~20 fields) + YAML marshalling, golden-tested
internal/network/           # subnet pool allocator (/24, jittered retry), allow-list parser
                            #   (network.allow grammar + deprecated-key translation), gateway
                            #   artifact generation (Corefile, dnsmasq.conf, nftables.rules)
internal/agents/            # agent registry: embedded builtins + ~/.hole/agents/<name>/ user agents,
                            #   command.json handling, build-context assembly
internal/image/             # canonical image config, manifest hashing, global-vs-project scope
                            #   decision, image GC (§11)
internal/sandbox/           # orchestration: identity (project name, instance id), startup sequence,
                            #   attach, idempotent teardown (shared by CLI + watchdog), -n dump
internal/state/             # instance registry: ~/.hole/instances/<instance>.json + docker labels;
                            #   powers `hole list`, GC, watchdog handoff
internal/watchdog/          # detached supervisor (hidden `hole __watchdog` command)
internal/dindregistry/      # pull-through registry container lifecycle
internal/hooks/             # setupHost/cleanupHost/prestart/setup resolution & execution
internal/worktree/          # git worktree detection and auto-library derivation
internal/update/            # version check (1s silent), self-update, legacy-resource migration
internal/logging/           # slog setup: console handler + per-run file handler, log-file GC
assets/                     # go:embed root: agents/, gateway/, schema/, dockerfiles
install.sh                  # rewritten: OS/arch detect, download binary + checksum, ~/.local/bin/hole
.github/workflows/ci.yml    # lint + unit (linux, macos) + integration/e2e (linux)
.github/workflows/release.yml  # version resolution (unchanged conventional-commit flow) + GoReleaser
```

Deleted at the end of the rewrite: `hole.sh`, `logger.sh`, `utils.sh`, `uninstall.sh`, all static
`docker-compose.yml` files, `proxy/`, `dns/` (replaced by embedded `gateway/`), root `schema/`
(moves under `assets/`).

## 4. CLI

Hand-rolled parser (not cobra/stdlib `flag`): the surface is small, and exact parity of today's
semantics — flags and positionals interleaved freely before `--`, everything after `--` verbatim
to the agent — is a requirement that interspersed-args frameworks make harder, not easier.

```
hole start <agent>[:<profile>] <path> [flags] [-- <agent args>]
hole list
hole destroy [<path>]
hole version | update | uninstall | help
hole __watchdog ...          # hidden, internal
```

- Flags kept: `-d/--debug`, `-n/--dump-network-access`, `-r/--rebuild`,
  `-u/--unrestricted-network`, `--with-docker`. `-d` + agent args still conflict.
- **New**: `--library <host_path>[:<container_path>][:rw]`, repeatable — ad-hoc library mounts
  merged *after* settings libraries (same resolution pipeline; default container path
  `/libs/<basename>`, read-only unless `:rw`).
- **New**: `hole list` — running sandboxes with instance ID, agent, profile, project path,
  uptime, DinD flag, network name, and which settings files were merged (from the state
  registry, cross-checked against live `docker ps` so dead entries are GC'd on the spot).
  Human-readable table output only for now; `--json` is a possible later addition.
- Profile syntax: split the agent positional on the **first** colon; a profile with any command
  other than `start` is a fatal error; empty (`claude:`) or pattern-violating names are fatal
  (details §10.3).
- **Bug fixes (deliberate behavior changes)**:
  1. `hole start <agent>` without a project path is an **error** (today it silently defaults
     to `.`). Explicitness is the point of the change requested in the analysis.
  2. `hole destroy <path>` actually uses the path. Today the positional lands in the agent
     slot and the **current directory's** project is destroyed instead; `hole destroy .` works
     only by accident. The Go parser gives `destroy` its own positional shape.

## 5. Behavior contract — edge-case inventory to preserve

The analysis requires reviewing every existing edge case. This is the checklist the port is
verified against (source: current `hole.sh` + developer docs). Each becomes a unit/integration
test where feasible.

**Identity & environment**

- Project name = sanitized basename + first 8 hex of sha1 of sanitized absolute path (stable);
  instance ID = 6 random `[a-z0-9]` via `crypto/rand` (replaces the WSL-sensitive
  `/dev/urandom | tr` pipeline — fix db3a279's class of bug disappears).
- Tmp dir under `~/.hole/tmp/run.XXXXXX`, **not** `$TMPDIR` — Colima/Lima/Podman-Machine VMs
  share `$HOME` but not `/var/folders`. Wiped last in teardown.
- Container user mirrors host: `$USER`, `$HOME` always; UID/GID **only on Linux** (Docker
  Desktop/OrbStack remap). `useradd -l` (sparse-lastlog guard for huge WSL UIDs), default
  `ubuntu` user removed, passwordless sudo, `BASH_ENV=~/.bash_env` for nvm-installed tools.

**Settings pipeline**

- Merge semantics: objects deep-merge project-wins, arrays concat + dedup preserving insertion
  order (global first), scalars project-wins. Exception: `agents.<name>.args` concatenates
  **without** dedup (§10.5).
- Path resolution pipeline for every path-valued setting: env expansion (`$VAR`/`${VAR}`,
  undefined → warn + leave literal, no infinite loop) → tilde (host `~/` vs container `~/` →
  `SANDBOX_HOME`) → relative-to-project → strip trailing slashes. One shared Go function; no
  hand-rolled path handling elsewhere (non-negotiable rule carries over).
- Error-handling split carries over verbatim: warn + skip for ignorable user-config problems
  (missing excluded path, glob with no matches, missing hook script, undefined env var);
  error + exit for anything making the sandbox wrong/unsafe (schema violation, unknown
  agent/profile, invalid allow entry, failed network allocation). Teardown is always
  best-effort, never aborts, and names every leftover it could not remove.

**Files & libraries**

- Exclusions: file → `/dev/null:<path>:ro`; dir → empty dir under tmp (never anonymous
  volumes); glob support incl. `**` (Go: `io/fs`-walk based matcher with globstar semantics —
  stdlib `filepath.Glob` has no `**`, so this is a small custom matcher with its own tests);
  dedup of overlapping matches; mirrored onto the DinD service.
- Includes: host→container map, container `~/` → sandbox home, relative container paths →
  project dir; nonexistent host path warn+skip. **New check** (§10.4): two includes resolving
  to the same container path → fatal, naming both sources.
- Libraries: string form = read-only; object form `{path, readwrite}`; container path must
  start `/`, `~`, `$` (schema); library's own `.hole/settings.json` honored for
  `files.exclude` **only**, validated, scoped to the library mount point.

**Sandbox lifecycle**

- Startup order: gateway (healthy) → DinD (if enabled, healthy) → agent; healthchecks via
  compose `depends_on: condition: service_healthy` (that's what Option A buys).
- Attach by exec-ing `docker attach <container>` with the inherited TTY (raw mode, resize,
  Ctrl-C proxying all remain the runtime CLI's problem — per the analysis's Option A rationale).
- Teardown phase order (idempotent, executed by the watchdog — §7.2, file-lock guarded): `-n` dump →
  compose down (label-based, `-p` only — works without the generated file) → explicit removal
  of both per-instance networks with attached-container force fallback → cleanupHost hooks
  (idempotent guard) → state-file removal → tmp dir last. Every failure logged with the
  leftover's name. Full determinism requirements in §7.4.
- Debug mode `-d`: command becomes `bash`; settings args unused; conflict with `--` args.
- `hooks.setupHost` failure aborts startup but cleanupHost still runs. Prestart scripts:
  copied with `001-` numbered prefixes, mounted RO, run in order, failure aborts. Setup hook
  content is part of the image hash (§11).
- DinD: privileged sidecar, `DOCKER_HOST=tcp://docker:2375`, project mounted at identical
  absolute path, exclusions mirrored, custom `environment` passed through, entrypoint wrapper
  keeps the stale `meta.db-lock`/`docker.pid` removal hack, containers reachable at hostname
  `docker`. Proxy env vars disappear (gateway world); registry mirror added (§8).

**Update / versioning**

- `version` file distinguishes installed vs dev build → in Go: version stamped via
  `-ldflags`/`debug.ReadBuildInfo`; dev builds (`hole version` → `development`) skip update
  checks exactly like today. Silent 1s-timeout release check on `start`/`version`.

## 6. Networking — transparent DNS-anchored filtering gateway

Replaces tinyproxy + standalone CoreDNS. The current model only covers HTTP/HTTPS from
proxy-aware clients; anything ignoring `HTTP_PROXY` (ssh, git over ssh, database clients, raw
sockets, UDP/QUIC) silently times out, and ports open globally (`ConnectPort`), never per
domain.

Confirmed requirements:

1. **Default deny.** With empty user settings, no traffic reaches the internet — except the
   built-in per-agent domains (each agent's `allow.txt`), which stay auto-allowed so the agent
   CLI itself works.
2. **All protocols, all ports.** TCP **and** UDP egress filterable, with no per-tool client
   configuration (no proxy env vars, no SOCKS wrappers).
3. **Rules are host × ports.** Users allow specific domains or IPs/CIDRs with specific ports.
4. **Explicit wildcard syntax.** `example.com` allows only that exact name; `*.example.com`
   allows subdomains. No implicit subdomain matching.
5. String shorthand settings syntax under a new `network.allow` key (§6.2).
6. Preserve: unrestricted mode (`-u`), `hostGatewayDomains`, the `-n` network access dump,
   DinD sidecar egress, podman compatibility.

### 6.1 Architecture

The `proxy` and `dns` services are replaced by a single `gateway` service that is
simultaneously the sandbox's DNS server, router, and firewall. Filtering moves from L7 (HTTP
proxy) to a DNS-anchored L3/L4 firewall:

```
agent ──(default route + DNS)──▶ gateway ──(masquerade)──▶ internet network
                                  │
                                  ├─ CoreDNS  :53   — DNS *policy*: answers only allowed
                                  │                   names (exact/wildcard), NXDOMAIN rest;
                                  │                   hostGatewayDomains → host gateway IP;
                                  │                   forwards allowed queries to dnsmasq
                                  ├─ dnsmasq  :5353 — resolves via 127.0.0.11 upstream and
                                  │                   writes answered IPs into nftables sets
                                  │                   (one set per unique port group)
                                  └─ nftables       — FORWARD chain: default drop; accept
                                                      only (daddr ∈ set, dport ∈ ports);
                                                      static sets for IP/CIDR entries;
                                                      masquerade on the internet interface
```

Enforcement is two independent layers:

- **DNS layer**: unknown names don't resolve at all (fast, clean failures — NXDOMAIN).
- **Firewall layer**: an IP:port is only reachable if the sandbox's own resolver handed that
  IP out for an allowed entry (or it is a static IP/CIDR entry). Hardcoded-IP bypass,
  third-party DNS (`dig @8.8.8.8`), and DoT are all dropped by default-deny.

**Why two DNS daemons in one container:** dnsmasq is the only mainstream resolver with native
nftables-set population (`--nftset=/domain/...`), but its domain matching is suffix-wide only —
`example.com` would also match `evil-sub.example.com`, violating requirement 4. CoreDNS sits in
front as the policy gate: its `view` plugin selects queries whose qname matches a generated
regex (exact names + explicit wildcards); everything else falls through to a catch-all NXDOMAIN
block. Since only policy-approved names ever reach dnsmasq, its broader suffix matching is
harmless — it only maps answered IPs to port sets. Both daemons run in the gateway container
because nftset writes to the kernel of the netns dnsmasq runs in — resolver and firewall must
share a network namespace.

**Traffic path:**

- The `sandbox` network stays `internal: true`. The gateway keeps the fixed-IP mechanism —
  `<subnet>.53` on the sandbox network (was the DNS container's IP); agent/DinD `dns:` entries
  point at it, `127.0.0.11` as fallback.
- Agent and DinD containers get their **default route replaced** to point at the gateway IP at
  startup (§6.4). The gateway has `net.ipv4.ip_forward=1` (compose `sysctls`) and
  `cap_add: NET_ADMIN`, masquerades sandbox-sourced traffic out the `internet` interface, and
  filters in the forward chain.
- Proxy env vars (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` + lowercase) are **removed** from the
  agent and DinD service definitions — no longer needed, and removing them is what makes
  non-proxy-aware tools work.
- IPv6: dropped entirely in nftables (`meta nfproto ipv6 drop` in the forward chain); CoreDNS
  answers empty AAAA for allowed names (mirrors existing hostGatewayDomains behavior).

### 6.2 Settings: `network.allow`

New array `network.allow`; entry grammar (string shorthand):

```
<host>[:<port>[,<port>...]]

host  := exact domain        example.com
       | wildcard domain     *.example.com        (subdomains only, not the apex)
       | IPv4 address        10.0.0.5
       | IPv4 CIDR           10.0.0.0/24
ports := integers 1-65535; omitted → default 443,80
```

```json
{
  "network": {
    "allow": [
      "api.github.com",
      "*.npmjs.org",
      "db.example.com:5432",
      "github.com:22,443",
      "10.0.0.5:22,2222",
      "192.168.1.0/24:8080"
    ]
  }
}
```

- **Ports apply to both TCP and UDP.**
- **Merge**: plain array — global + project concatenated and deduplicated (generic merge, no
  special casing).
- **Schema**: `network.allow` with a validation `pattern` for the grammar (schema stays strict).
- **Validation**: parse each entry in Go; malformed entry → fatal (a wrong allow-list makes the
  sandbox unsafe/wrong — not a skippable warning).

**Settings migration (2.0 is a breaking release):**

- `network.domainWhitelist` + `network.allowedPorts` are **removed** in 2.0. They are dropped
  from the schema, but Hole detects them *before* validation and fails with a targeted
  migration error that prints the suggested equivalent, ready to paste into `network.allow`:
  each `domainWhitelist` entry `d` maps to `d` and `*.d` with ports = merged `allowedPorts`
  (or default 443,80), matching tinyproxy's old unanchored-regex behavior; `allowedPorts: []`
  (old `ConnectPort 0`) maps to "no ports". The translation logic survives only as this
  hint generator — never as silent runtime behavior.
- Builtin agents' tinyproxy-regex `allowed-domains.txt` files are converted once to the new
  shorthand as embedded `allow.txt` files (`api\.anthropic\.com` → `api.anthropic.com`,
  `.*\.anthropic\.com` → `*.anthropic.com`; default ports 443,80). The empty shared base file
  disappears. Custom user agents (§9) use the same `allow.txt` format.
- `network.hostGatewayDomains` keeps its key and semantics and gains an **optional
  `:port[,port...]` suffix now** — backward compatible: an entry without a suffix keeps the
  all-ports behavior (host services are explicitly user-configured and typically run on
  arbitrary dev ports); a suffixed entry restricts the firewall allow to those ports. Schema
  pattern extended accordingly; plain-domain validation and the `localhost`/`127.0.0.1`
  warning stay. Each entry resolves to the host gateway IP (CoreDNS, as today — zone
  matching, so subdomains included).

### 6.3 Gateway component (embedded `assets/gateway/`)

**Dockerfile**: Alpine + dnsmasq + nftables + the pinned CoreDNS release binary (reuse the
`COREDNS_VERSION` build-arg mechanism). Healthcheck: DNS probe against CoreDNS (replaces
`nc -z localhost 8888`). Image is shared, not per-project: `hole-sandbox/gateway:latest`
(build inputs never depend on settings — the three config files below are runtime mounts).
Restart policy `on-failure` (crash resilience without resurrecting orphans after daemon
restarts — §7.4).

**Generated per-run artifacts** (produced by `internal/network` from the parsed allow-list
model — Go structs + `text/template`, golden-tested; written to the run tmp dir and
bind-mounted read-only). Hole parses the full merged allow list (agent files + translated
deprecated keys + `network.allow` + hostGatewayDomains) into rule groups keyed by **unique
port set**, then generates:

1. **`Corefile`** — policy front-end: one `view`-guarded root block
   (`expr name() matches '<generated-regex>'` → `forward . 127.0.0.1:5353`); the regex is
   built from exact names (escaped, anchored) and wildcards (`([^.]+\.)+domain\.tld`), plus
   hostGatewayDomains blocks answering the host gateway IP (the `{HOST_GATEWAY_IP}`
   placeholder + entrypoint substitution mechanism carries over); a catch-all root block
   returning NXDOMAIN (`template` plugin); `log` plugin on both blocks (powers `-n`, §6.5).
2. **`dnsmasq.conf`** — enforcement back-end: listen on `127.0.0.1:5353`; upstream
   `127.0.0.11`; per rule group one line per domain: `nftset=/<domain>/4#inet#hole#<set>`;
   `log-queries` off (CoreDNS logs already).
3. **`nftables.rules`** — one `inet hole` table: per rule group a dynamic ipv4_addr set
   populated by dnsmasq plus a static set for literal IP/CIDR entries; forward chain, policy
   **drop**: accept `ct state established,related`; accept new connections where
   `daddr ∈ <set>` and `(tcp dport ∈ ports or udp dport ∈ ports)`; accept all ports to the
   host-gateway IP when hostGatewayDomains are configured; count/log denied packets
   (`counter log prefix "hole-denied " limit rate ...`); input chain accepts DNS (53 tcp+udp)
   from the sandbox subnet, drops other new sandbox-side input; postrouting `masquerade` on
   the internet interface; `meta nfproto ipv6 drop`.

**Entrypoint**:

1. Resolve `host.internal` (via `extra_hosts: host-gateway`) and substitute
   `{HOST_GATEWAY_IP}` in Corefile and nftables rules.
2. Detect sandbox-side and internet-side interfaces by matching interface IPs against the
   known sandbox subnet (passed as env var) — eth0/eth1 ordering is not guaranteed by compose.
3. `nft -f` the ruleset; start dnsmasq (background); exec CoreDNS.

### 6.4 Agent and DinD route injection

- Agent service gets `cap_add: [NET_ADMIN]`; all proxy env vars removed.
- Agent entrypoint: before prestart hooks, `sudo ip route replace default via ${GATEWAY_IP}`
  (gateway IP passed as env var; the agent user has passwordless sudo, `cap_add` grants the
  capability to root processes).
- DinD sidecar: already `privileged: true`; its generated service definition wraps the stock
  entrypoint with the same route replacement and loses its proxy env vars. Non-Hub registry
  domains still must be user-allowed.
- Startup order: gateway (healthy) → docker (if enabled, healthy) → agent.

**Why NET_ADMIN on the agent is safe:** the sandbox network is `internal: true`; the only path
to the internet is *through* the gateway, and all filtering happens on the gateway. Deleting or
rewriting routes inside the agent can only break its own connectivity, never widen it. (The
agent user already has sudo by design — the container is the sandbox boundary, not the user.)

### 6.5 Network access dump (`-n`)

Tinyproxy's graceful-stop + `docker cp` log scrape is replaced by two domain-level sources on
the gateway, both from `docker logs <instance>-gateway-1`:

- **ALLOWED**: CoreDNS query-log lines from the allowed view (names actually resolved).
- **DENIED**: CoreDNS query-log lines answered NXDOMAIN by the catch-all block.

Extracted, deduped, sorted into the same
`<project>/.hole/logs/network-access-<agent>-<instance_id>.log` format. Firewall-level denials
of direct-IP attempts don't appear in DNS logs; the nftables denied counter/log exists for
debugging (`nft list` from a debug shell) but is not part of the v1 dump — documented
limitation.

### 6.6 Unrestricted mode (`-u`)

- CoreDNS: single root block, `forward . 127.0.0.1:5353` unconditionally.
- nftables: forward chain policy `accept` (masquerade still required).
- dnsmasq: plain forwarder (no nftset lines).

### 6.7 Security notes and accepted limitations

- **Shared/rotating CDN IPs**: once an allowed domain resolves to an IP, that IP:port stays
  reachable for the sandbox lifetime (nftset entries never expire). An agent could reach a
  *different* domain on the same IP by connecting directly. Accepted for v1; future hardening
  is SNI verification on TLS flows.
- **CNAME chains**: dnsmasq adds all addresses from the chased answer, so CDN-fronted domains
  work without allowing the CDN vendor explicitly.
- **DNS bypass attempts**: queries to external resolvers (53/853 to arbitrary IPs) are dropped
  by default-deny; DoH only works toward already-allowed IPs (equivalent to the CDN limitation).
- **UDP**: stateful via conntrack, same accept-rule shape as TCP. QUIC to an allowed host:443
  works.

### 6.8 Subnet allocation and network capacity

New in the Go version, replacing the current probe-network trick (create a throwaway network,
read the subnet Docker's IPAM picked, recreate with `--subnet`), which leaks, races, and burns
a Docker-default-pool /16 per network — ~14 sandboxes exhaust the pool:

- Dedicated pool, default `10.222.0.0/16`, overridable via `network.subnetPool` (schema +
  validation floor `/23` — a `/24` pool passes trivial validation but can never start a
  sandbox, since each instance needs two /24s; the exhaustion error must state pool capacity
  so "too small" is distinguishable from "full").
- Per instance, two `/24`s allocated by Hole: `<instance>_sandbox` (internal) and
  `<instance>_internet` (plain bridge, gateway egress). Both pre-created via
  `network create --subnet` with labels, referenced `external: true` in the generated compose.
- Allocation: list all existing Docker networks' subnets in one pass; attempt 1 takes the
  lowest free /24 (predictable for single sandboxes); on an overlap rejection (`network
  create` is atomic — concurrent starts race benignly), retry with a **random** free candidate,
  bounded ~10 attempts. Strict first-fit on retries makes concurrent starts stampede the same
  candidate (prototyping on the bash codebase showed 12 simultaneous first-fit starts
  exhausting the retry budget, while with jitter 20 succeed with 20 unique subnets).
- Gateway fixed IP stays `<subnet base>.53` inside the /24. A stale same-name network from a
  crashed run is removed before creation.
- Capacity: ~127 concurrent sandboxes (2 × /24 from a /16) — well above the required 20.
  Docker's own default pools are untouched. Document the VPN/LAN-overlap caveat for the
  default pool.
- **New requirement — network name propagation**: agent and DinD containers get
  `HOLE_SANDBOX_NETWORK=<instance_name>_sandbox` in their environment (some tools need it,
  e.g. to `docker network connect` from DinD; hooks can key off it).

## 7. Sandbox tracking, watchdog, GC (`hole list` + reliable cleanup)

### 7.1 State registry

`cmd start` writes `~/.hole/instances/<instance_name>.json` before creating any Docker
resource: instance ID/name, project path + name, agent, profile, flags, merged settings source
files, a snapshot of the merged settings (for cleanupHost in the watchdog path), CLI PID,
watchdog PID, network names/subnets, DinD enabled, started-at, hole version. Every Docker
resource additionally carries labels (`hole.managed=true`, `hole.instance=<name>`,
`hole.project=<project_name>`) — labels are the ground truth for GC and `destroy`; the JSON
file is the metadata cache for `hole list` and the watchdog's work order.

### 7.2 Watchdog — the sole runtime executor of teardown

Immediately after the state file is written, the CLI spawns `hole __watchdog <instance_name>`
detached (setsid, stdio → the run's log file). The watchdog — not the CLI — performs teardown
in **every** runtime case; the CLI only waits on it and mirrors its output. This makes the
cleanup path single-owner and continuously exercised (the code that runs after `kill -9` is
the exact code that runs on every clean exit), and makes teardown immune to terminal
lifecycle: it runs in a setsid'd process, so closing the tab or a second Ctrl-C cannot
interrupt it halfway.

Watchdog logic:

1. Until the agent container exists, watch the CLI PID (a startup abort before the agent
   starts must still trigger teardown of partial resources — the CLI also pings the watchdog
   explicitly on early failure so there is no polling lag in the common failure case).
2. Once the agent container exists, `docker wait` it. Agent exited — for any reason: clean
   exit, CLI signaled, CLI killed, terminal closed, host slept — → acquire the per-instance
   teardown file-lock → run the idempotent teardown (`internal/sandbox`, phases in §5) →
   remove the state file → exit. cleanupHost hooks run here, fed by the settings snapshot in
   the state file; they execute **without a TTY** (output is relayed via the log, interactive
   scripts are unsupported) — a semantic change to document in the README.
3. `kill -9` of *both* CLI and watchdog: covered by GC (§7.3) — the analysis's "there CANNOT
   be any container or network left after sandbox exit" is guaranteed by the watchdog for
   every single-process-death mode, and by GC for the double-death mode.

CLI side, after `docker attach` returns (or startup fails):

- **Wait + relay**: tail the run's log file, mirroring the watchdog's teardown progress to the
  console (the `-n` dump location, hook failures, "Sandbox destroyed"), until the state file
  is gone. The user's prompt returns only when resources are actually gone — UX identical to
  today, and no race with an immediate re-start.
- **Fallback**: if the watchdog PID is dead while the sandbox still exists (watchdog crash/
  OOM), the CLI runs the same shared teardown function inline. This is the only case where
  the CLI tears anything down itself.

### 7.3 GC (on every `start` and `list`, best-effort, each removal logged)

- Networks: prune `hole.managed`-labeled networks that are unattached **and** older than a
  safety threshold (~10 min) — the age check protects a concurrent start that created its
  network but hasn't started containers yet (use the runtime's own prune filters
  `label=` + `until=`, not hand-rolled date math).
- Containers: remove stopped `hole-sandbox-*` containers whose compose project has **no
  running containers and no networks left** — the network check matters because compose has a
  window where it has created but not yet started its first container; without it GC would
  reap a concurrent start out from under it.
- Volumes: remove orphaned per-instance DinD volumes whose instance has neither a network nor
  a container left (volume prune has no `until` filter — liveness of sibling resources is the
  age proxy; this is why instance volumes are created **after** network creation in the
  startup sequence, so a mid-startup instance is always recognizable).
- Abandoned instances: a state file whose CLI PID **and** watchdog PID are both dead → full
  teardown including *running* containers. The state file is what lets GC distinguish an
  abandoned instance from a concurrent healthy one — a distinction the bash version cannot
  make, which is why `kill -9` orphans are unrecoverable there.
- `~/.hole/tmp/run.*` dirs older than one day (a long-lived legitimate session is protected by
  its instance still being alive); run-log files past retention (§12).
- Superseded images per §11.5.

### 7.4 Deterministic teardown requirements

The current bash teardown has known root-cause defects behind the "cleanup seems random"
symptom; the Go design must fix all of them from day one:

- Teardown must be keyed on the instance name alone, registered **before** network creation —
  never gated on "all services started". (The bash bug: `compose down` only ran if the final
  `up -d agent` had succeeded; any earlier failure leaked gateway/dns containers, and the
  network removal then failed invisibly because containers were still attached.)
- CLI signal handling is minimal by design: INT/TERM/HUP → stop the agent container (which
  triggers the watchdog's `docker wait`), enter the wait+relay loop, exit with conventional
  codes (130/143/129). No second-Ctrl-C protection is needed in the CLI — teardown runs in
  the detached watchdog, which terminal signals cannot reach; the watchdog additionally
  ignores INT/TERM during teardown for robustness.
- `compose down --remove-orphans` runs fileless (`-p <instance>` only — compose v2 resolves
  from container labels), so teardown never depends on the generated compose file still
  existing.
- Both per-instance networks are removed explicitly (compose does not remove external
  networks). If removal fails because containers are still attached (partial down), fall back:
  inspect attached container IDs → `rm -f` them → retry once.
- No blanket error suppression: every failed removal logs a warning naming the exact leftover;
  a final verification step checks nothing matching the instance remains and prints the manual
  cleanup command if it does. Bounded retry (one) around `compose down`.
- Teardown is one shared, file-lock-guarded, idempotent function — the watchdog calls it in
  the normal path, the CLI in the watchdog-dead fallback, GC for abandoned instances. The
  lock is a reentrancy guard, not a coordination mechanism: there is exactly one intended
  executor at any time.

## 8. DinD pull-through registry (replaces volume cache)

- A single long-running container `hole-registry` (upstream `registry:2` image, config
  `proxy.remoteurl: https://registry-1.docker.io`), storage in volume `hole-registry-data`,
  labeled `hole.managed`. Started lazily on the first DinD-enabled `start` if absent; left
  running between sandboxes (that is its point); removed by `hole uninstall` (and
  `hole destroy` with no path). It sits on its own small bridge network with normal egress —
  it only ever talks to Docker Hub, and it is host-side infrastructure, not part of any
  sandbox's trust boundary.
- At sandbox start (DinD enabled): `docker network connect <instance>_sandbox hole-registry`
  so the DinD daemon can reach it *without* internet access; disconnect during teardown
  (best-effort). DinD daemon gets `--registry-mirror=http://hole-registry:5000`; sandbox-
  internal traffic is not filtered (filtering is on the gateway's forward chain to the
  internet only).
- **Deleted**: `hole-sandbox-docker-cache` volume, per-instance seed/sync, flock serialization
  and its cache-wipe race handling. Each DinD still gets a fresh named volume per instance for
  `/var/lib/docker` (concurrent sandboxes must not share it), but it is now purely disposable
  — removed in teardown, cache hits come from the mirror.
- Limitation documented in README: the mirror caches **Docker Hub only**; other registries
  (ghcr, ECR) go through the gateway and need `network.allow` entries, uncached. A follow-up
  can add per-registry mirrors via generated DinD `daemon.json` if demand appears.
- Migration: first run of the Go version removes the legacy cache volume (§14).

## 9. Agents — builtins + user-defined

- Builtin plugins (claude, gemini, codex) live embedded under `assets/agents/<name>/` with the
  same file contract as today: `command.json` (required), `allow.txt` (required, §6.2 format),
  `install-root.sh` / `install-user.sh` (optional).
- **New**: user agents in `~/.hole/agents/<name>/` with the identical contract, discovered at
  startup. `hole start my-agent .` just works. Rules: name pattern `^[a-z0-9][a-z0-9-]*$`
  (colon-free — profile syntax), a user agent name colliding with a builtin is a fatal error
  (no silent shadowing of e.g. `claude`), missing `command.json` is fatal at start of that
  agent, user agents participate in `container.enabledAgents` and allow-list merging exactly
  like builtins (every enabled agent's `allow.txt` merges, regardless of which agent starts).
- Schema consequence: `container.enabledAgents` and `agents.<name>` can no longer be a closed
  enum — becomes the name pattern; membership validated at runtime against the registry
  (better error message anyway).
- The unified-image build keeps its phase order (base → remove ubuntu user → CACHEBUST → base
  deps + EXTRA_PACKAGES + docker CLI → root installs → user creation → user installs → setup
  hooks → entrypoint). Enabled agents' install scripts (builtin from embed, user from disk)
  are copied into the build context; their content is part of the image hash (§11.2), so
  editing a user agent's install script auto-invalidates the image.
- This also gives the test suite a **test agent** for free: e2e tests register a trivial
  bash-loop agent under the test home dir and never need real agent CLIs or API keys.

## 10. Profiles

Named settings overlays defined inside the existing `settings.json` files (global and
per-project) and selected at start time via `hole start <agent>:<profile> <path>`.

Agreed requirements: profiles live under a top-level `profiles` key; each profile value uses
the same schema as root settings minus `profiles` itself (no nesting); no profile selected →
exactly today's behavior; **additive-only** merge (deliberate decision, §10.4); composition via
`extends` in config, not runtime multi-selection.

Non-goals (explicitly out of scope): selecting multiple profiles at once (`claude:a,b`) — the
name pattern reserves `,` and `:` so the syntax could be extended later; a `--profile` flag or
`HOLE_PROFILE` env var; a per-project default-profile setting; profiles in library settings
files (schema-valid, ignored — libraries only honor `files.exclude`; documented).

### 10.1 Settings format

```json
{
  "network": { "allow": ["api.github.com"] },
  "profiles": {
    "research": {
      "network": { "allow": ["*.wikipedia.org"] }
    },
    "docker": {
      "container": { "docker": true },
      "dependencies": ["make"]
    },
    "research-docker": {
      "extends": ["research", "docker"],
      "environment": { "MODE": "research-docker" }
    }
  }
}
```

**Profile name pattern** `^[a-z0-9][a-z0-9-]*$`: excludes `:` (CLI syntax) and `,` (future
multi-profile reserve), and is directly usable in image naming with no transformation.
Enforced twice: by the schema (`propertyNames`) for defined profiles, and by the CLI parser
for the requested name (fast, clear failure even when no settings file exists).

### 10.2 Schema restructure

To avoid duplicating every property definition (draft 2020-12):

- All current root properties except `$schema` move into `$defs/settings` (an object schema
  **without** `additionalProperties: false` — strictness moves to call sites via
  `unevaluatedProperties`, which, unlike `additionalProperties`, sees properties evaluated
  through `$ref`).
- Root: `$ref: #/$defs/settings` + own properties `$schema` and `profiles`
  (`propertyNames` pattern; each value `$ref: #/$defs/settings` +
  `unevaluatedProperties: false` + an `extends` property: `string | string[]`, items matching
  the name pattern) + `unevaluatedProperties: false`.
- Effects: root accepts everything it does today plus `profiles`; a profile accepts exactly
  the root settings keys but not `profiles` (no nesting) and not `$schema`; unknown keys
  anywhere still fail validation. Nested `additionalProperties: false` inside sub-objects
  stays as-is. `santhosh-tekuri/jsonschema` fully supports this (it is the `jv` engine).
- Cycle/existence checks for `extends` are runtime checks (JSON Schema cannot express them).

### 10.3 CLI parsing and validation

- Split the agent positional on the **first** colon: `claude:research` → agent `claude`,
  profile `research`. `validate_agent` runs on the split-off agent part unchanged.
- Profile given with a command other than `start` → fatal
  ("profiles can only be used with the start command").
- Empty after colon (`claude:`) or pattern-violating name → fatal with the allowed pattern in
  the message.
- **Profile existence check** (after validation, before merging): a requested profile must
  exist in `.profiles` of **at least one** of the two files, else fatal + a listing of the
  profile names available in each file. Rationale: a silently ignored profile would run the
  sandbox with the wrong permissions — that makes the sandbox wrong, so it is fatal per the
  error-handling convention. Defined in only one file is fine.
- Startup banner gains `Profile: <name>` when one is selected (omitted otherwise); help text
  documents `hole {command} {agent}[:profile] {path}` with an example.

### 10.4 Merge algorithm and additive-only semantics

Merge order (lowest → highest; later wins on scalars, arrays concat + dedup, objects
deep-merge — identical to the standard semantics):

1. global base (global file without `profiles`)
2. global profile overlays, in chain order (§10.6)
3. project base (project file without `profiles`)
4. project profile overlays, in chain order

Invariant preserved: anything in the project file always overrides anything global. The
`profiles` key (and `extends` metadata) is stripped unconditionally, so the merged result
never contains it — no downstream consumer needs changes and the no-profile case degenerates
to today's two-way merge. A missing overlay is `{}`.

**Additive-only is a deliberate decision, not a limitation**: profiles can only *add* to the
base, never narrow it. Replace/remove semantics were considered and rejected — effective
permissions would require mentally evaluating subtraction across four sources. Two documented
consequences (README Profiles section):

1. **Minimal-base pattern**: to serve least-privilege-per-task, the base stays minimal and
   each profile adds only what its mode needs. A broad base cannot be narrowed by a profile.
2. **Varying a mount between profiles**: `files.include` is keyed by *host* path, so a base
   mount `~/.claude → ~/.claude` cannot be *replaced* by a profile mount
   `~/claude-review → ~/.claude` — both survive the merge and target the same container path.
   Rule of thumb: a mount whose source varies between profiles must live inside each profile,
   not in the base. Scalar settings don't have this problem (profile value wins).
   A startup check makes the collision case actionable: two resolved includes targeting the
   same container path → fatal, naming both host sources.

### 10.5 New setting: `agents.<name>.args`

Default CLI arguments for an agent, usable in base settings and profiles alike (the original
"different flags per mode" motivation):

```json
{ "profiles": { "opus": { "agents": { "claude": { "args": ["--model", "opus"] } } } } }
```

- **Schema**: top-level `agents` object in `$defs/settings`; property names follow the agent
  name pattern (open — custom agents, §9); each value `{ "args": string[] }`, strict.
- **Command construction**: agent service command = base command (`command.json`) + merged
  `agents.<agent>.args` + CLI args after `--`. CLI args come last so an ad-hoc flag overrides
  a settings-provided value flag (last-one-wins). Only the *started* agent's args apply.
- **Merge exception — no dedup**: generic array dedup would corrupt argument vectors
  (`["--allowedTools", "a", "--allowedTools", "b"]` would lose the second `--allowedTools`).
  After the generic merge, each `agents.<name>.args` is recomputed as a plain concatenation
  (no dedup) of all contributing sources in standard order — generalized to the N sources
  produced by chain expansion. Concatenation keeps the additive philosophy; last-one-wins
  gives profiles a natural override for value flags.
- **Debug mode `-d`**: command is `bash`; settings args unused, no error.
- **Trust note** (README): a project's `.hole/settings.json` can inject agent flags. No new
  exposure — project settings already control mounts and `hooks.setupHost` (host-side script
  execution), which are strictly more powerful.

### 10.6 Profile inheritance (`extends`)

- `extends`: string or array of profile names; metadata, stripped before merging.
- **Chain resolution**: the selected profile expands depth-first into an ordered name list —
  parents before children, in listed order, each name applied **once** (visited-set dedup, so
  diamonds are harmless under additive merge). `research-docker` above →
  `[research, docker, research-docker]`.
- **Cross-file resolution**: a profile name may be defined in both files; its effective
  `extends` is the standard array merge of both definitions (global first, concat, dedup).
  Chain expansion runs on this combined view, so a project profile can extend a
  globally-defined one and vice versa.
- **Application order** preserves "project beats global": global base → global overlays in
  chain order → project base → project overlays in chain order; the leaf profile remains the
  highest-precedence overlay within each file.
- **Errors (fatal)**: unknown parent (message lists available profiles per file); inheritance
  cycle (message names the cycle path).
- Runtime selection unchanged: always exactly one profile name; image identity uses the merged
  result (§11), so caching stays unambiguous.

### 10.7 Edge cases (test checklist)

| Case | Behavior |
|---|---|
| No profile | Unchanged; degenerates to two-way merge |
| Profile in both files | Four-way merge as specified |
| Profile in only one file | Works; missing overlay `{}` |
| Profile in neither file | Fatal + list of available profiles |
| `claude:` / `claude:Foo` / `claude:a:b` / `claude:a,b` | Fatal: invalid name |
| Profile with non-`start` command | Fatal |
| No settings files exist but profile requested | Fatal (existence check) |
| `profiles` inside a profile | Rejected by schema |
| `profiles` in a library settings file | Schema-valid, ignored, documented |
| Same profile name, different content per file | Both overlays apply; project wins |
| Base + profile mount different hosts to same container path | Fatal after include resolution, both sources named |
| Profile needs to *narrow* base permissions | Unsupported — additive-only; minimal-base pattern documented |
| `agents.claude.args` in base and profile | Concatenated, no dedup, base first |
| Settings args + CLI `--` args | Both apply; CLI last (final say) |
| `agents.gemini.args` set but claude started | Ignored |
| Settings args + `-d` | Args unused, no error |
| Extends unknown profile / cycle `a→b→a` | Fatal (list / cycle path) |
| Diamond (`d` extends `b`,`c`; both extend `a`) | `a` applied once |
| `extends` differs global vs project for same profile | Effective = array-merged view |
| Parent defined only in the other file than the child | Works (combined view) |
| cleanupHost after a profiled run | Uses the profile-merged settings snapshot |

## 11. Shared agent images (config-hash tags, global vs project scope)

Today every project gets its own `hole-sandbox/{agent,proxy,dns}-<project>:latest` even though
content is usually identical; rebuilds never benefit other projects; and changing
image-affecting settings does **not** rebuild (compose reuses an existing tag) until the user
remembers `-r`. The Go version ships hash-tagged images **from day one** — the
`:latest`-per-project scheme is never ported.

### 11.1 Classification: image-affecting vs runtime-only settings

| Setting | Affects image? | How |
|---|---|---|
| `container.baseImage` | **yes** | `BASE_IMAGE` build arg |
| `dependencies` | **yes** | `EXTRA_PACKAGES` build arg |
| `container.enabledAgents` | **yes** | which agents' install scripts enter the build context |
| `hooks.setup` scripts | **yes** | script **content** copied into build context |
| `files.*`, `libraries` | no | runtime volume mounts |
| `network.*` | no | runtime gateway config mounts |
| `environment` | no | compose `environment` |
| `container.memoryLimit` / `memorySwapLimit` | no | compose `mem_limit` / `memswap_limit` |
| `container.docker` / `--with-docker` | no | sidecar only (Docker CLI always baked in) |
| `hooks.prestart` | no | runtime RO mount |
| `hooks.setupHost` / `cleanupHost` | no | host-side |

Non-settings inputs that must be part of the cache identity: host user identity
(`SANDBOX_USERNAME`/`SANDBOX_HOME` always, UID/GID on Linux — two host users sharing a daemon
must not collide) and Hole's own build inputs — in Go these are the embedded assets
(Dockerfile, entrypoint, builtin agent install scripts), whose hash is a compile-time constant
of the binary version, plus **user agent files from `~/.hole/agents`** hashed at runtime.

### 11.2 Canonical image config and tag

A helper reduces a settings document to its **canonical image configuration** — normalized
JSON with defaults applied, so "explicitly set to the default" and "not set" are
indistinguishable:

```
{
  "baseImage":      container.baseImage or "ubuntu:24.04",
  "enabledAgents":  normalized via the same resolver the build uses (absent/empty → all),
  "dependencies":   post-merge order preserved (exactly the EXTRA_PACKAGES order),
  "setupScriptShas": content sha1 of each resolved setup script, in run order;
                     missing file normalizes to absent (matching warn+skip behavior)
}
```

Serialized with sorted keys, compact — string equality is the comparison. Critical invariant:
normalization **reuses the exact code paths the build uses** (enabled-agents resolver,
dependencies extraction, shared path pipeline for script resolution) — otherwise the scope
decision and the actual build context can diverge.

Hashing setup-script **content** (not paths) gives correct behavior for free: a project
overriding with a different script → different sha → project image; pointing at an identical
copy → same sha → shared image; a *relative* script path in global settings resolves against
the project dir, so its content legitimately differs per project — captured automatically.

**Image tag** = first 12 hex chars of sha1 over a manifest of: (1) the canonical config,
(2) host identity, (3) Hole build-input hashes (embedded-assets hash + user-agent file
hashes for enabled user agents, deterministic order, absent files hashed as the literal
`absent`). `CACHEBUST` is deliberately **not** in the manifest — it is a rebuild trigger, not
configuration.

Consequence: any change to an image-affecting input produces a new tag → tag missing →
`compose up` builds automatically. `-r` is no longer needed after settings changes; it remains
the way to refresh *versions* (latest apt packages / agent CLIs) under an unchanged config.

### 11.3 Scope decision: global vs project image

Resolved in `cmd start` after settings merge:

```
merged_cfg = canonical(merged settings, project_dir)
global_cfg = canonical(global settings only, project_dir)

merged_cfg == global_cfg  →  hole-sandbox/agent-global:<hash>
otherwise                 →  hole-sandbox/agent-<project_name>:<hash>
```

`global_cfg` is the global file (or `{}`) run through the same canonicalization — *not* a
"does the project file contain these keys" check. This implements "project image only if the
project actually modifies the image": runtime-only project settings → shared global image;
a project repeating global values verbatim (or a dependency subset that dedups away) → shared
global image; new dependency / different setup content / different enabledAgents or baseImage
→ project image.

With a profile selected, the "global" baseline is `canonical(global base + global profile)` —
a *global* profile still counts as global, and a profile that only adds runtime settings keeps
the shared image. (This subsumes the older per-profile image-suffix idea entirely.)

Naming safety: `project_name` always ends in an 8-hex path-hash suffix, so no project can
collide with the literal `agent-global` repository; host identity is in the hash, so two host
users on a shared daemon get distinct tags. Two projects of the same user with identical
custom config still get separate project repositories — deliberate, to keep
`hole destroy <path>` semantics trivial; cross-project dedup of custom configs is already
~free at the Docker layer level.

Logging: the start banner names the image with scope and, for project scope, the differing
keys (computed by comparing the two canonical configs):

```
Agent image: hole-sandbox/agent-myproj-1a2b3c4d:9f8e7d6c5b4a (project-specific: dependencies, setupScript)
Agent image: hole-sandbox/agent-global:0123456789ab (shared)
```

The gateway image is shared and settings-independent: `hole-sandbox/gateway:latest` (its
config files are runtime mounts; it changes only with a Hole release or dev edits, covered by
`-r`). Hash-tagging it would be machinery for no realistic staleness.

### 11.4 `-r/--rebuild` semantics

- Remove the **resolved** agent tag and `hole-sandbox/gateway:latest`, best-effort (`rmi`
  fails if another running sandbox uses the image; compose `--build` then re-tags and the old
  image becomes dangling, reclaimed by the labeled prune later).
- Export `CACHEBUST` so everything after the Dockerfile's `ARG CACHEBUST` re-runs.
- UX win: `-r` in any project refreshes the shared image for **all** projects using it.

### 11.5 Image GC

Run **after** `compose up` of the agent succeeds (never before — a failed build must not have
destroyed the last working image), and from `destroy`. All removals best-effort; an image in
use by a concurrent sandbox survives until a later pass.

1. Remove all other tags of the chosen repository (keep only the current tag) — bounds each
   repository to one live tag per config generation.
2. If the **global** image was chosen: also remove all tags of the project's own agent
   repository (the project previously needed a custom image and no longer does).
3. Dangling reclamation: all Hole Dockerfiles carry `LABEL com.hole.image=<agent|gateway>`;
   run `image prune --force --filter "label=com.hole.image"` (dangling-only by default, so
   tagged images are never touched, and the label guarantees Hole never prunes a user's
   unrelated dangling images). Verify the combined filter on podman; if unsupported, skip
   with a debug warning.

`destroy <path>`: remove **all** tags of `hole-sandbox/agent-<project_name>` (reference
filter); keep `agent-global`, `gateway` (may serve other projects; log says so). `destroy`
(no path) and uninstall: `reference=hole-sandbox/*` catches everything.

Accepted trade-offs: concurrent first starts of two projects resolving to the same missing
shared tag both build; the second tag write wins and the twin dangles (label-pruned later) —
rare and harmless; serializing builds was rejected (couples unrelated sandboxes' startup
latency). `enabledAgents` cannot *narrow* an explicitly-listed global set (concat+dedup merge
— today's documented semantics); canonicalization mirrors it by construction.

## 12. Other configuration changes, logging

- **Setup hook becomes an array with patterns** (analysis requirement): `hooks.setup` accepts
  `[{"script": ".hole/setup.d/*.sh"}, ...]` — array merged global-first like setupHost; each
  entry a literal path or a glob (matches sorted lexicographically, so `001-`/`002-` chunking
  works); resolved scripts copied into the build context as `setup-scripts/NNN-name.sh` and
  run in order during the build. The old scalar object form (`{"script": ...}`) is
  **removed** in 2.0 — the schema rejects it, and the same pre-validation migration check as
  §6.2 names the array replacement. All script contents feed the image hash (§11.2).
- **Host/runtime hook scripts accept patterns too** (non-breaking): the same literal-or-glob
  entry resolution is one shared code path applied to `hooks.setupHost`, `hooks.cleanupHost`
  and `hooks.prestart` as well — an entry containing glob metacharacters is expanded (after
  the standard path pipeline), matches run sorted lexicographically in place of the entry,
  and a pattern matching nothing is warn+skip. No schema change (entries stay strings).
  Non-breaking by construction: in the current version a path containing glob characters
  always failed the file-exists check and was warned+skipped, so no working configuration
  changes meaning.
- **Git worktrees**: new setting `git: { "worktreeLinks": "ro" | "rw" | "off" }` (default
  `"ro"`). On start, if `git` is on PATH: project is a linked worktree → auto-add the main
  repo as a library at its own absolute path; project is a main repo → auto-add each linked
  worktree whose path is outside the project dir. Auto-libraries behave exactly like
  configured ones (incl. their own `.hole/settings.json` exclusions); explicit
  `libraries`/`--library` entries for the same host path win. `git` missing or not a repo →
  skip silently.
- **Schema changes summary** (embedded schema, strictness preserved): `profiles` + `extends` +
  `agents` (§10), `network.allow` + `network.subnetPool` (§6), `hooks.setup` array form,
  `git` block, `enabledAgents` enum → pattern (§9). Removed in 2.0 (with targeted migration
  errors, §6.2): `network.domainWhitelist`, `network.allowedPorts`, `hooks.setup` object form.
- **Logging**: `log/slog` with two handlers — console (colored, level-filtered, today's
  message style; the UX must not regress) and a per-run JSON file
  `~/.hole/logs/run-<date>-<instance_id>.log` (debug level always, includes engine command
  lines + durations). Watchdog logs to the same file. Retention (~7 days) rides the startup
  GC. Secrets discipline: engine logging redacts `environment` values; merged settings never
  logged verbatim above debug level. The `-n` dump keeps its separate
  `<project>/.hole/logs/network-access-*.log` contract.

## 13. Testing & CI (analysis: "automated testing usable by agents and CI")

- **Unit** (no Docker): merge semantics incl. profiles/extends/args-no-dedup (§10.7 table);
  path pipeline incl. undefined-var behavior; exclusion glob matcher; hook-entry glob
  resolution (literal vs pattern, sort order, no-match warn+skip); allow-grammar parser +
  deprecated-key translation; subnet allocator (overlap against supernets, nested subnets,
  unaligned pool base, exhaustion, `/23` floor, octal-looking input); compose model golden
  files (minimal, DinD, memory limits, debug, mounts); gateway artifact golden files
  (Corefile/dnsmasq/nftables per rule-group scenario); image canonicalization + hash
  stability + scope decisions; CLI parsing table tests (both fixed bugs, `--` handling,
  `--library` forms, `agent:profile`).
- **Integration** (`-tags integration`, real Docker, Linux CI): network create/remove/force
  fallback + stale same-name replacement; every GC branch incl. keep-conditions (§7.3);
  watchdog matrix — clean exit (watchdog cleans, CLI relays and returns only after resources
  are gone), SIGINT/SIGTERM/SIGHUP during build, terminal closed mid-teardown (teardown
  completes detached), `kill -9` of CLI (watchdog cleans), `kill -9` of the watchdog while
  the sandbox runs (CLI fallback cleans), `kill -9` of CLI+watchdog (next start's GC cleans),
  startup abort before the agent container exists (watchdog reaps partial resources);
  allocator race
  (~20 simultaneous allocations → unique subnets) and an exhaustion regression loop
  (~40 sequential cycles, zero leak, same /24 reused); registry container lifecycle; state
  registry + `hole list`; teardown idempotence and early-failure paths.
- **E2E** (test agent from §9, no API keys): full start→attach→exit→zero-leftovers, plus the
  gateway functional matrix from inside the sandbox:
  - empty settings: `curl https://example.com` fails with NXDOMAIN (not timeout); agent CLI
    still reaches its built-in domains
  - exact domain works on 443/80, subdomain NXDOMAIN; wildcard: subdomain works, apex NXDOMAIN
  - custom port `db.example.com:5432`: psql-style connect works, 443 to same host refused
  - `github.com:22`: `git clone git@github.com:...` with no client proxy config
  - IP and CIDR entries with ports; direct-IP connect to a non-allowed IP dropped
  - UDP to allowed host+port; QUIC/HTTP3 to allowed :443
  - bypass attempts blocked: `dig @8.8.8.8`, hardcoded-IP HTTPS, DoT :853
  - hostGatewayDomains: host service reachable on an arbitrary port without a suffix; with a
    `:ports` suffix only the listed ports get through
  - DinD: `docker pull` via the mirror; non-Hub pull blocked without an allow entry
  - `-u` lets everything flow; `-n` dump correct in both modes
  - removed keys (`domainWhitelist`/`allowedPorts`, scalar `hooks.setup`) fail fast with the
    migration hint naming the `network.allow` / array equivalents
  - profiles (incl. §10.7 rows that need a live sandbox), worktree auto-linking, shared-image
    scope walkthrough (fresh state → global image; runtime-only project settings → still
    shared; add a dependency → project image with reason logged; revert → project repo GC'd;
    global dependency edit → all shared projects rebuild once)
- **CI**: `golangci-lint`; unit on ubuntu + macos runners; integration+e2e on ubuntu (Docker
  preinstalled). Podman parity job (rootless) at least for: allocator, `network create
  --internal --subnet --label`, `network prune --filter label= --filter until=`, fileless
  `compose -p down`, gateway `sysctls`/`cap_add`, `network connect` for the registry, image
  prune label filter. `make test` / `make itest` entry points documented so coding agents run
  the right suite.
- Platform verification CI cannot cover (Docker Desktop, OrbStack, Colima on macOS; WSL)
  stays a documented manual checklist per milestone — notably the gateway spike (§15 Phase 0).

## 14. Install / update / uninstall / release

- **install.sh** (rewritten, still `curl | bash`): detect `uname -s`/`-m` → download
  `hole_<os>_<arch>` + `checksums.txt` from the latest GitHub release → verify → install to
  `~/.local/bin/hole`. `~/.local/share/hole/` is no longer needed.
- **`hole update`**: go-selfupdate — compare against latest release, download, verify
  checksum, atomic replace of the running binary (no uninstall/reinstall exec dance).
- **Migration & resource hygiene** (analysis requirement): the binary stores
  `~/.hole/state.json` with the last-run version. On first run of a new version (including
  first run ever of the Go version over a bash install): remove superseded hole Docker
  resources — legacy per-project `hole-sandbox/{agent,proxy,dns}-*` images, legacy
  `hole-sandbox-docker-cache` + agent-home volumes, orphaned networks — and remove the old
  bash install (`~/.local/share/hole/`, old wrapper) if present. Logged, best-effort.
- **`hole uninstall`**: removes all `hole.managed`-labeled + `hole-sandbox-*`-named resources
  (incl. registry container + volume), `~/.hole` (asks about settings, as befits user data),
  and the binary itself.
- **Release workflow**: keep the trigger (push to `main`) and conventional-commit version
  resolution (`codacy/git-version`, `feat:` → minor); the resolved version becomes a tag that
  GoReleaser builds: `linux_amd64`, `linux_arm64` (covers WSL), `darwin_amd64`,
  `darwin_arm64`, checksums, release-drafter notes as today. The packaging `cp` list dies.

## 15. Documentation plan

Per repo rules, docs move with the code in the same PRs:

- `README.md`: full rewrite of Installation (binary install, no jq/jv), Update, network
  section (`network.allow`, default-deny, wildcard rules, deprecations, DinD registry note),
  Profiles (concept, merge order, additive-only + minimal-base pattern + per-profile mount
  guidance, `extends`), agent args (+ trust note), custom agents, worktrees, `hole list`,
  `--library`, logging locations, and the hooks section note that cleanupHost scripts now run
  without a TTY (§7.2).
- `documentation/developer/`: architecture (Go process model, watchdog, state registry,
  single generated compose file, engine package), networking (gateway — full §6 content),
  agents (registry + user agents), configuration (new merge pipeline, image-affecting
  classification), guidelines (**replaced**: Go conventions — gofmt/golangci-lint, package
  layout, error-handling policy mapping warn-skip/fatal to Go errors, "all engine calls in
  internal/engine", asset embedding rule), recipes (add agent / setting / asset; run tests;
  debug — updated triage strings, e.g. the pool-exhaustion message), build-and-release
  (GoReleaser flow).
- `.claude/CLAUDE.md`: tech stack, code guidelines (bash rules → Go rules), build/test
  commands, non-negotiable rules updated (release.yml packaging rule → embed rule; schema
  rule unchanged; path-pipeline rule unchanged; cleanup rule unchanged).
- **`MIGRATION.md`** (repo root, linked prominently from the README and the v2.0.0 release
  notes): the 1.x → 2.0 upgrade guide. Contents:
  - how to upgrade (run `install.sh` once — `hole update` of the bash 1.x cannot self-migrate
    to a binary; the old install and its Docker resources are cleaned automatically on the
    Go version's first run, §14) and what disappears from the host (`jq`, `jv`, `sha1sum`,
    tarball install dir no longer needed)
  - settings changes: removed keys and their `network.allow` / `hooks.setup`-array
    replacements with before/after JSON examples (mirroring the built-in migration errors,
    §6.2, §12); pointer to the new keys (`profiles`, `agents`, `git`, `network.subnetPool`)
  - behavior changes to expect: project path now required on `start`; `hole destroy <path>`
    honors the path; default-deny networking replacing the HTTP-proxy model (proxy env vars
    gone — tools no longer need proxy awareness); `-r` no longer needed after settings
    changes (§11); DinD cache now a pull-through registry (first pull re-warms it, old cache
    volume removed); cleanupHost hooks run without a TTY (§7.2)
  - a short troubleshooting section: what the migration errors look like and how the `-n`
    dump helps rebuild an allow list from observed traffic.

## 16. Phased implementation order (each phase = reviewable PR series to `dev`)

**Phase 0 — Scaffold + gateway spike** (parallelizable)
- Go module, CI (lint+unit), logging, `internal/engine` skeleton with runtime detection,
  embedded assets pipeline, version stamping.
- **Gateway spike** (risk burn-down, gates Phase 1): hand-built gateway container proving
  CoreDNS-view → dnsmasq-nftset → nftables forward filtering + agent route injection on
  Docker Desktop, OrbStack, and rootless podman.

**Phase 1 — Core vertical slice (parity-equivalent `start`/`destroy`)**
- config load/validate/merge (no profiles yet), path pipeline, identity, subnet allocator
  (§6.8), compose generation, unified image build **directly with hash tags** (§11.2; scope
  decision may hard-code "project" until Phase 3), gateway service with internally-translated
  legacy whitelist sources (behavior-compatible milestone covering HTTP/HTTPS), agent/DinD
  route injection, start sequence, attach, in-process idempotent teardown (§7.4),
  `destroy`/`destroy all`, `-d`/`-r`/`-u`/`-n`, hooks (setupHost/cleanupHost/prestart/setup
  scalar), DinD sidecar (legacy volume cache untouched for now), e2e harness with the test
  agent.
- Milestone: a user can replace the bash version for daily work.

**Phase 2 — Reliability layer**
- Labels everywhere, state registry, watchdog, startup GC (§7.3), `hole list`, teardown lock,
  kill-matrix integration tests.

**Phase 3 — Feature wave** (independent PRs, any order)
- `network.allow` + migration errors for removed keys + hostGatewayDomains `:ports` suffix +
  agent `allow.txt` conversion (completes §6)
- Profiles + `extends` + `agents.<name>.args` (§10)
- Shared-image scope decision (global vs project) + image GC (§11.3, §11.5)
- `hooks.setup` array + glob patterns; pattern support for setupHost/cleanupHost/prestart entries
- `--library` flag; required-path + destroy-path bug fixes land here at the latest
- Custom agents (`~/.hole/agents`), schema enum → pattern
- Git worktree auto-libraries
- DinD pull-through registry, deletion of the volume-cache path (§8)
- `HOLE_SANDBOX_NETWORK` propagation

**Phase 4 — Distribution**
- GoReleaser + release workflow rewrite, install.sh rewrite, self-update, version-change
  migration/cleanup (incl. bash-era resource removal), uninstall.

**Phase 5 — Cutover**
- Docs rewrite (§15), delete bash sources, final full test matrix incl. manual platform
  checklist, merge `dev` → `main` — released as **`v2.0.0`** (the bash line is 1.x; the Go
  rewrite is a breaking major release: runtime dependencies change, settings keys removed).
- **Final step — migration documentation**: write `MIGRATION.md` (contents specified in §15)
  and wire it in: linked from the README top, referenced from the v2.0.0 release notes, and
  named by the in-product migration errors (§6.2) so a failing 1.x settings file points the
  user at the guide. Verified by walking a real 1.x setup (settings with `domainWhitelist`,
  scalar `hooks.setup`, DinD cache volume, bash install dir) through the documented steps.

## 17. Risks

| Risk | Mitigation |
|---|---|
| Gateway (nftables in Docker Desktop/OrbStack/Colima VMs, rootless podman netns) | Phase-0 spike gates everything. All ship Linux ≥5.x kernels where nftables is standard; rootless podman permits nft as the userns owner. Fallback: dnsmasq `--ipset` + iptables-legacy variant of the generated ruleset |
| Gateway interface detection (eth0/eth1 ordering) | Match by IP/subnet, not name (§6.3); in the test matrix |
| CoreDNS `view` plugin availability | Stock builds since 1.10; pin `COREDNS_VERSION` accordingly |
| Tools that *only* work via proxy env vars | None known — transparent routing is a strict superset; a stub HTTP proxy on the gateway could be reintroduced behind a setting if a regression appears |
| `podman compose` behavioral drift (fileless `down`, labels, healthcheck gating, `sysctls`/`cap_add`, `network connect`, prune filters) | Podman CI job from Phase 1; engine package isolates every call site |
| Watchdog portability (setsid/daemonize on macOS, WSL PID semantics) | stdlib-only detach (`SysProcAttr{Setsid: true}`); PID liveness via signal 0; kill-matrix tests on both CI OSes, manual on WSL |
| Registry mirror insufficient for non-Hub registries | Documented limitation; per-registry mirrors as follow-up; DinD still functional without cache |
| Self-update edge cases (read-only install path, future package managers) | go-selfupdate handles common cases; on failure print the install.sh one-liner |
| Scope creep: parity + gateway + profiles + shared images + new features in one rewrite | Phase gating with a usable milestone at end of Phase 1; every Phase-3 item independently shippable |
| Config compatibility regressions | §5 inventory + §10.7 table enforced as test checklists; 2.0 is deliberately breaking for the removed keys — targeted migration errors (§6.2) instead of silent behavior changes |

## 18. Resolved questions (decision record)

1. **Deprecated keys**: removed outright in 2.0 (`network.domainWhitelist`,
   `network.allowedPorts`, scalar `hooks.setup`) — no deprecation period; targeted migration
   errors with paste-ready equivalents instead (§6.2, §12).
2. **`hostGatewayDomains` ports**: the optional `:port[,port...]` suffix is implemented now.
   Backward compatible — an entry without a suffix keeps the all-ports behavior (§6.2).
3. **nftset entry TTL/expiry**: not implemented — sandboxes may live for a long time and
   expiry would break long-lived connections on re-resolution. The CDN-IP window remains an
   accepted limitation (§6.7) until SNI verification lands.
4. **`hole list` output**: human-readable table only for now; `--json` is a possible later
   addition (§4).
5. **Registry container removal**: `hole destroy` (no path) removes it along with everything
   else — it is a cache, cheap to rebuild; `hole uninstall` removes it too (§8).
6. **Cutover version**: `v2.0.0` — the bash line is 1.x; the Go rewrite is a breaking major
   release (runtime dependencies change, settings keys removed) (§16 Phase 5).
