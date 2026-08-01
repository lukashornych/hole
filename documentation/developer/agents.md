# Agents

Hole supports multiple AI agent CLIs through a per-agent plugin structure under `agents/`.
All **enabled** agents are installed into a single unified sandbox image; the agent named on the
command line (`hole start <agent> ...`) only determines the container's startup command.

The list of supported agents is defined in three places that must stay in sync (see
[recipes — add a new agent](recipes.md#add-a-new-supported-agent)):

- `VALID_AGENTS` array in `hole.sh`
- the `container.enabledAgents` enum in `schema/settings.schema.json`
- the `agents/<agent>/` directory itself

## Per-agent directory layout

```
agents/<agent>/
  install-root.sh       # optional — runs as root during image build (system packages)
  install-user.sh       # optional — runs as the agent user during image build (CLI itself)
  command.json          # required — startup command as a JSON array of argv parts
  allowed-domains.txt   # required — tinyproxy regex patterns the agent CLI needs
```

- `command.json` is read by `get_agent_base_command()` and emitted as the compose `command`.
  Arguments after `--` on the Hole command line are appended. Entries may reference `$HOME`
  (e.g. the gemini/codex commands invoke a pinned nvm node binary). Commands run the agents in
  their "skip permissions / yolo" modes — the sandbox is the safety boundary, not the agent's
  own permission prompts.
- `install-user.sh` typically also pre-seeds config so the user is not re-prompted in every
  sandbox (e.g. claude writes `~/.claude.json` with onboarding/trust flags accepted).
- `allowed-domains.txt` patterns from **all enabled agents** are merged into the proxy
  whitelist, regardless of which agent is started
  (see [networking — whitelist merging](networking.md#whitelist-merging)).

## Unified agent image (`agents/Dockerfile`)

Built per project as `hole-sandbox/agent-${PROJECT_NAME}:latest` (`pull_policy: never`).
Build phases, in order:

1. **Base image**: `ubuntu:24.04` by default; overridable via `container.baseImage` build arg
   (must remain Ubuntu 24.04-based — the Dockerfile uses apt and removes the default `ubuntu`
   user).
2. **Default `ubuntu` user removal** — its UID (1000) is usually needed for the agent user.
3. **`CACHEBUST` arg** — everything after this line re-runs on `--rebuild` so setup scripts and
   package installs pick up changes/latest versions.
4. **Base dependencies** (curl, git, jq, nano, vim, ripgrep, ca-certificates, gnupg, sudo), then
   user-defined `EXTRA_PACKAGES` (from the `dependencies` setting), then Docker CLI + compose
   plugin (for the DinD sidecar).
5. **Root-phase agent installs**: every `agent-installs/<agent>/install-root.sh` copied into the
   build context (only enabled agents are copied there by `generate_instance_compose()`).
6. **Agent user creation**: `AGENT_USERNAME`/`AGENT_HOME`/`UID`/`GID` build args mirror the host
   user (`useradd -l` avoids a huge sparse lastlog for very high host UIDs); passwordless sudo.
7. **User-phase agent installs** run as the agent user, with `BASH_ENV=~/.bash_env` set up so
   tools installed via nvm & co. work in non-interactive shells.
8. **User setup hooks** (`hooks.setup`, copied to `setup-scripts/001-name.sh`, `002-name.sh`, ...)
   run as the agent user in sorted order; script content changes bust the Docker layer cache.
9. **Entrypoint**: `agents/entrypoint.sh` — runs any mounted `/tmp/prestart-scripts/*` in sorted
   order (numbered `001-`, `002-`, ... prefixes; failure aborts startup), then `exec "$@"` into
   the agent command.

## Container user identity

The container user mirrors the host: username from `$USER`, home path from `$HOME`, and on
Linux also UID/GID (`SANDBOX_UID`/`SANDBOX_GID` exports — Docker Desktop and OrbStack remap IDs
automatically, so this is Linux-only). Because the home path matches the host's, agent config
paths (e.g. `~/.claude/`) look identical inside and outside the sandbox, which makes
`files.include` mappings like `"~/.npmrc": "~/.npmrc"` straightforward.

The home directory is baked into the per-project image during build (agent CLIs, `.bashrc`,
pre-seeded config). It is **not** persisted across sandbox runs — persistent state must be
brought in via `files.include` mounts.
