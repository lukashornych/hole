# Configuration (`settings.json`)

Sandbox behavior is configured via two optional files sharing one schema
(`schema/settings.schema.json`):

- `~/.hole/settings.json` — global defaults
- `<project>/.hole/settings.json` — per-project settings

User-facing reference and examples live in the [README](../../README.md#configuration); this
page documents the mechanics — validation, merging, and how each setting maps to Docker
resources in `generate_instance_compose()`.

## Validation

`validate_settings()` runs `jv` (santhosh-tekuri/jsonschema CLI) against
`schema/settings.schema.json` for every settings file that exists — global, project, and each
library's own settings file. Validation failure aborts startup with the validator's error lines.
The schema uses `additionalProperties: false` throughout, so any new setting **must** be added
to the schema or every user's startup breaks
(see [recipes](recipes.md#add-a-new-settings-option)).

## Merge semantics

`merge_settings()` deep-merges global + project JSON with an embedded `jq` program:

- **Objects**: recursively merged; for scalar conflicts the **project value wins**
- **Arrays**: concatenated (global first, then project) and deduplicated preserving insertion
  order
- **Scalars**: project overrides global

The merged JSON is held in a shell variable and queried with `jq -r` per setting — it is never
written to disk.

## Path resolution

All path-valued settings (`files.include` keys, `files.exclude`, `libraries` keys,
`hooks.*.script`) go through the same pipeline:

1. **Environment variable expansion** (`expand_env_vars`): `$VAR` and `${VAR}` via bash indirect
   expansion (`${!var_name}`) — no `eval`. Undefined variables produce a `log_warn` and are left
   unexpanded literally.
2. **Tilde expansion**: `~/...` → `$HOME/...` on the host side; container-side paths expand
   `~/` against `SANDBOX_HOME`.
3. **Relative path resolution**: against the project directory.
4. **Trailing slashes** are stripped.

Non-existent host paths generally produce a `log_warn` and the entry is skipped
(startup continues); see [guidelines — error handling](guidelines.md#error-handling).

## Settings reference (implementation view)

### `files.exclude` — secret hiding

`resolve_file_exclusions()` turns each entry into an over-mount:

- **Files** → `/dev/null:<project_dir>/<path>:ro`
- **Directories** → an empty host dir created under `${HOLE_TMP_DIR}/excluded-dirs/...` and
  bind-mounted over the target. Avoids anonymous Docker volumes (which `compose down` leaks
  without `-v`); wiped with `HOLE_TMP_DIR` on exit.
- Entries containing `*`, `?` or `[` are treated as globs and expanded against the project dir
  in a subshell with `globstar nullglob` (`**` matches recursively). Patterns matching nothing
  → warning, skipped.
- Overlapping entries are deduplicated — each resolved path is mounted once.

When DinD is enabled, the same exclusion volumes are mirrored on the `docker` sidecar so
`docker build` inside the sandbox cannot see the secrets either.

### `files.include` — extra mounts

Object of `host_path → container_path`. Host path resolved as above; container path supports
`~/` (sandbox home), absolute, `$VAR`, or relative (resolved against the project dir — i.e. same
path inside the sandbox). Each entry becomes a read-write bind mount on the agent service.

### `libraries` — sibling/dependency mounts

Object of `host_path → container_path` (string form, read-only) or
`host_path → { "path": ..., "readwrite": true }`. Host path must be an existing directory.
If the library itself contains `.hole/settings.json`, only its `files.exclude` entries are
honored — resolved against the library source dir and mounted scoped to the library's container
mount point. Container paths must start with `/`, `~` or `$` (enforced by the schema).

### `network.domainWhitelist`, `network.allowedPorts`, `network.hostGatewayDomains`

See [networking](networking.md) — whitelist merging, generated `ConnectPort` list, and the
CoreDNS host-gateway setup.

### `dependencies` — apt packages

Joined into the `EXTRA_PACKAGES` build arg and installed in a conditional `RUN` layer of
`agents/Dockerfile`; baked into the per-project cached image, so subsequent starts are instant.
Supports version pinning (`pkg=version`). Because apt runs during `docker build` (host
networking), the Ubuntu repos do **not** need to be on the proxy whitelist.

### `environment` — custom env vars

Object of `NAME → value`; injected into the agent service's `environment` section, and also
passed to the DinD sidecar when Docker is enabled.

### `container.*`

- `baseImage` → `BASE_IMAGE` build arg (default `ubuntu:24.04`; must stay Ubuntu 24.04-based)
- `memoryLimit` / `memorySwapLimit` → compose `mem_limit` / `memswap_limit` on the agent service
- `enabledAgents` → which agents' install scripts and whitelists are baked in/merged
  (default: all of `VALID_AGENTS`). The startup agent must be in this list.
- `docker` → enables the DinD sidecar (equivalent to the `--with-docker` flag)

### `hooks.*` — lifecycle scripts

| Hook | Where | When | Runs as | On failure |
|---|---|---|---|---|
| `hooks.setup.script` | container (build) | during `docker build`, after agent installs | agent user | aborts build |
| `hooks.prestart[]` | container (runtime) | every start, before the agent CLI (`entrypoint.sh`) | agent user | aborts startup |
| `hooks.setupHost[]` | host | before any Docker work in `cmd_start` | host user | aborts startup (cleanupHost still runs) |
| `hooks.cleanupHost[]` | host | teardown phase 5, after Docker teardown | host user | warning only, teardown continues |

Implementation notes:

- `setup.script` is a scalar (project overrides global); the script is copied into the build
  context as `setup-scripts/setup.sh`, so content changes bust the Docker layer cache.
- `prestart` is an array (global entries first, then project); scripts are copied to
  `${HOLE_TMP_DIR}/prestart-scripts/` with numbered prefixes (`001-name.sh`, ...) and mounted
  read-only at `/tmp/prestart-scripts/`; `entrypoint.sh` runs them in sorted order.
- `setupHost`/`cleanupHost` run with the caller's shell environment plus Hole's exported vars.
  `run_cleanup_host_hooks` is idempotent (`_CLEANUP_HOST_DONE` guard) because the EXIT trap may
  re-enter.
- Missing script paths → `log_warn`, entry skipped.

## Docker-in-Docker (DinD) sidecar

Enabled by `container.docker: true` or `--with-docker`:

- The agent image always contains the Docker CLI + compose plugin; only the sidecar is
  conditional.
- The generated override adds a privileged `docker:dind` service with proxy env vars, the
  project mount at the host absolute path, mirrored exclusion volumes, custom `environment`
  entries, and a `docker info` healthcheck. Startup order: proxy healthy → docker healthy →
  agent.
- The agent gets `DOCKER_HOST=tcp://docker:2375` (no TLS — internal network only) and `docker`
  appended to `NO_PROXY` so Docker CLI traffic does not route through the HTTP proxy.
- **Layer cache persistence**: a shared `hole-sandbox-docker-cache` volume survives across runs.
  Each run seeds a per-instance `hole-sandbox-docker-data-<instance>` volume from the cache
  (concurrent sandboxes must not share `/var/lib/docker`), and teardown phase 4 syncs the
  instance data back to the cache under a `flock` and removes the instance volume.
- Registry domains (e.g. `registry-1.docker.io`) must be whitelisted by the user via
  `network.domainWhitelist`.
