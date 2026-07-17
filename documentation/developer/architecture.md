# Architecture

Hole is a single-process bash CLI (`hole.sh`) that orchestrates a multi-container sandbox via
Docker Compose (or Podman Compose). There is no daemon: `hole start` builds/starts the sandbox,
attaches the terminal to the agent container, and an `EXIT` trap destroys everything when the
agent CLI exits.

## CLI entry point

`hole.sh` → `main()`:

1. Parses arguments in a single loop. Flags before `--` belong to Hole
   (`-d/--debug`, `-n/--dump-network-access`, `-r/--rebuild`, `-u/--unrestricted-network`,
   `--with-docker`); everything after `--` is passed verbatim to the agent CLI.
2. Positional arguments: `command` (`start`, `destroy`, `update`, `uninstall`, `version`, `help`),
   `agent` (`claude`, `gemini`, `codex` — from `VALID_AGENTS`), `path` (defaults to `.`).
3. Dispatches to `cmd_start`, `cmd_destroy` / `cmd_destroy_all` (no path given), `cmd_update`,
   `cmd_uninstall`, `cmd_version`.

### Sandbox identity

- **Project name** (`create_project_name_from_project_path`): sanitized project dir basename +
  `-` + first 8 hex chars of the sha1 of the sanitized absolute path. Stable per project —
  used for cached image names (`hole-sandbox/agent-${PROJECT_NAME}:latest`, `.../proxy-...`,
  `.../dns-...`) and for `hole destroy <path>` resource filtering.
- **Instance ID** (`generate_instance_id`): 6 random `[a-z0-9]` chars from `/dev/urandom`.
  Unique per run — multiple sandboxes of the same project can run concurrently.
- **Instance name**: `hole-sandbox-<project_name>-<instance_id>`. Used as the compose project
  name (`-p`), the sandbox network name (`<instance_name>_sandbox`), and container name prefix.

### Container runtime detection

`detect_container_runtime()` resolves the runtime in priority order: `$HOLE_RUNTIME` env var →
`docker` → `podman` → error. It also verifies `<runtime> compose version` works. The resolved
command is stored in the `CONTAINER_RUNTIME` global (one of the few intentional globals; see
[guidelines](guidelines.md)).

### Temp directory

Every `start` creates `HOLE_TMP_DIR` via `mktemp -d "${HOME}/.hole/tmp/run.XXXXXX"`. It lives
under `$HOME` (not `$TMPDIR`) so Colima/Lima VMs on macOS — which share `$HOME` but not
`$TMPDIR` — can bind-mount files from it. It holds all generated per-run artifacts:

- `docker-compose.yml` — generated compose override
- `tinyproxy.conf` + `tinyproxy-domain-whitelist.txt` — generated proxy config
- `Corefile` + `dns-entrypoint.sh` — DNS build context
- `entrypoint.sh`, `agent-installs/`, `setup-scripts/` — agent image build context
- `prestart-scripts/` — numbered prestart hook scripts (mounted read-only)
- `excluded-dirs/` — empty dirs bind-mounted over excluded directories

The whole directory is wiped in the last cleanup phase.

## Compose file layering

`create_compose_cmd()` assembles one compose invocation from five files, later files overriding
earlier ones:

```
docker-compose.yml            # base: defines networks (sandbox, internet)
proxy/docker-compose.yml      # proxy service
dns/docker-compose.yml        # dns service
agents/docker-compose.yml     # agent service (project mount, proxy env vars)
${HOLE_TMP_DIR}/docker-compose.yml   # generated per-run override
```

The generated override (`generate_instance_compose()`) contributes everything derived from
merged settings and flags: build contexts/args, exclusion/inclusion/library volumes, environment
variables, memory limits, the agent command (from `command.json` + args after `--`, or `bash` in
debug mode), proxy/DNS config mounts, the optional DinD sidecar, and the external sandbox
network reference.

Runtime variables consumed by the static compose files are exported by `cmd_start`:
`PROJECT_NAME`, `PROJECT_DIR`, `SANDBOX_USERNAME`, `SANDBOX_HOME`, `SANDBOX_UID`/`SANDBOX_GID`
(Linux only — Docker Desktop/OrbStack handle ID mapping themselves), and `CACHEBUST` /
`SANDBOX_REBUILD` when `--rebuild` is used.

## Container architecture

**Two-network design for security:**

- `sandbox` network: `internal: true` (no direct internet access) — where the agent runs.
  It is **pre-created by `create_sandbox_network()`** outside compose (declared `external` in the
  override) because assigning a fixed DNS IP requires an explicit subnet: Docker's IPAM picks a
  free subnet via a temporary probe network, then the network is re-created with `--subnet` set.
  The DNS container gets the fixed address `<subnet base>.53` (`compute_dns_ip_from_subnet`).
- `internet` network: plain bridge with internet access — joined only by `proxy` and `dns`.

**Services:**

- `proxy`: tinyproxy on port 8888, filters requests against the merged domain whitelist.
  See [networking](networking.md).
- `dns`: CoreDNS; resolves user-configured `hostGatewayDomains` to the Docker host gateway and
  forwards everything else to Docker's embedded DNS (`127.0.0.11`). See [networking](networking.md).
- `agent`: unified agent container (Ubuntu 24.04 by default) with all enabled agent CLIs
  installed. The startup agent selects the container command. See [agents](agents.md).
- `docker` (optional): `docker:dind` sidecar when Docker-in-Docker is enabled.
  See [configuration — Docker-in-Docker](configuration.md#docker-in-docker-dind-sidecar).

## Startup sequence (`cmd_start`)

1. Detect container runtime; create `HOLE_TMP_DIR`; register `trap '_cleanup_sandbox' EXIT`.
2. Validate global (`~/.hole/settings.json`) and project (`.hole/settings.json`) settings with
   `jv` against `schema/settings.schema.json`; deep-merge them with `jq`
   (see [configuration](configuration.md#merge-semantics)).
3. Verify the startup agent is in `container.enabledAgents`.
4. Export runtime variables for compose (see above). On `--rebuild`, export `CACHEBUST` and
   remove old per-project images so no dangling images accumulate.
5. `check_for_update` (silent, 1s timeout — skipped in dev checkouts without a `version` file).
6. If DinD is enabled: ensure the shared `hole-sandbox-docker-cache` volume exists and seed a
   per-instance `hole-sandbox-docker-data-<instance>` volume from it.
7. Run `hooks.setupHost` scripts on the host (before any Docker work; failure aborts startup).
8. Create the sandbox network and compute the DNS IP.
9. Generate the compose override; assemble the compose command.
10. Start services in order: `dns` → `proxy` (waits for healthcheck) → `docker` (if enabled) →
    `agent`.
11. Set the `_CLEANUP_*` state variables so the EXIT trap performs full teardown.
12. `docker attach <instance>-agent-1` — the user now talks to the agent CLI. When the agent
    exits (or the script receives a signal), the EXIT trap tears everything down.

## Teardown (`_cleanup_sandbox`)

A single idempotent EXIT-trap handler; state is passed via `_CLEANUP_*` globals set during
`cmd_start` (a deliberate exception to the "no globals" rule — traps cannot receive arguments).
Phases, in order:

1. **Network access log dump** (only with `-n`): stop the proxy gracefully so tinyproxy flushes
   its log, `docker cp` the log out, extract distinct `ALLOWED`/`DENIED` domains into
   `<project>/.hole/logs/network-access-<agent>-<instance_id>.log`.
2. **`compose down --remove-orphans`** — must run before the temp dir is removed because the
   compose files live there.
3. **Remove the external sandbox network** (compose does not remove external networks).
4. **DinD volume sync**: copy instance Docker data back to the shared cache volume (serialized
   with `flock`), then remove the instance volume.
5. **Run `hooks.cleanupHost` scripts** on the host (failures logged as warnings, never abort).
6. **Remove `HOLE_TMP_DIR`** (always last).

## Destroy commands

- `hole destroy <path>` (`cmd_destroy`): stops/removes containers, networks, cached
  `hole-sandbox/{agent,proxy,dns}-<project_name>` images and orphaned DinD instance volumes for
  one project.
- `hole destroy` (`cmd_destroy_all`): removes **all** containers, images, networks and volumes
  matching the `hole-sandbox` prefixes.

## Security model

**Network isolation:**

- The agent container sits on an internal network and cannot reach the internet directly.
- All HTTP/HTTPS traffic is routed through the proxy via `HTTP_PROXY`/`HTTPS_PROXY` env vars
  set in `agents/docker-compose.yml`.
- The proxy enforces a domain whitelist merged from: default (`proxy/allowed-domains.txt`,
  empty) → all enabled agents' `allowed-domains.txt` → user `network.domainWhitelist` →
  `network.hostGatewayDomains`.
- CONNECT is limited to ports 80/443 unless overridden via `network.allowedPorts`.

**File access control:**

- The project directory is mounted read-write at the **same absolute path** as on the host
  (`${PROJECT_DIR}:${PROJECT_DIR}`), and it is the container working dir.
- Secrets are hidden by over-mounting: files get `/dev/null:<path>:ro`, directories get an
  empty host dir from `${HOLE_TMP_DIR}/excluded-dirs/` bind-mounted over them.
- Extra mounts (`files.include`, `libraries`) are opt-in; libraries are read-only by default.

**Agent runs as non-root:**

- The container user mirrors the host user (`$USER`, `$HOME`, and on Linux the host UID/GID) so
  files created in the project mount have correct ownership. The user does have passwordless
  `sudo` inside the container (the container is the sandbox boundary, not the user).
