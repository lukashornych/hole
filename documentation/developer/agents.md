# Agents

Hole supports multiple agent CLIs through a plugin structure. Builtin agents are embedded in the
binary; user agents live on disk. Both use the identical contract, so a user agent is a
first-class agent everywhere — including allow-list merging, `container.enabledAgents` and image
identity.

All **enabled** agents are installed into a single sandbox image. The agent named on the command
line only decides the container's startup command.

## Plugin contract

```
assets/agents/<name>/      (builtin, embedded)
~/.hole/agents/<name>/     (user)
  command.json       # required — startup command as a JSON array of argv parts
  allow.txt          # required — domains the agent CLI needs at runtime
  install-root.sh    # optional — runs as root during the image build (system packages)
  install-user.sh    # optional — runs as the sandbox user during the build (the CLI itself)
```

- `command.json` is emitted as the compose `command`. Entries may reference `$HOME`, which Hole
  expands to the sandbox home before writing the compose file — the gemini and codex commands
  use it to invoke a pinned nvm node binary. Commands run the agents in their
  "skip permissions / yolo" modes: the sandbox is the safety boundary, not the agent's own
  prompts.
- `allow.txt` uses the allow-list shorthand (`<host>[:<port>,...]`, default ports 443 and 80),
  with `#` comments. Entries from **every enabled agent** are merged regardless of which agent
  starts, so switching agents inside one image needs no settings change.
- `install-user.sh` typically also pre-seeds config so the user is not re-prompted in every
  sandbox — claude writes `~/.claude.json` with the onboarding and trust flags accepted.

> **The Node version is pinned in two files that must agree.** gemini and codex name the
> interpreter by absolute path (`$HOME/.nvm/versions/node/v<x.y.z>/bin/node`) because the agent is
> `exec`'d without nvm's shell initialisation, so `install-user.sh` must run `nvm install <x.y.z>`
> for exactly that version. A floating `nvm install 22` drifts to a newer patch at build time and
> the agent then fails to launch with ENOENT — invisible until someone starts it. Changing the
> version means editing both files; `TestPinnedNodeVersionMatchesTheInstallScript` fails if they
> disagree.

## Registry rules

- Names must match `^[a-z0-9][a-z0-9-]*$`. Colon-free, because the CLI splits the agent
  positional on the first colon to select a profile.
- A user agent whose name collides with a builtin is **fatal**. Silently shadowing `claude` would
  change what a well-known command starts.
- A missing `command.json` fails when that agent is started, not at registry load: one broken
  plugin directory must not stop every other agent from working.
- `container.enabledAgents` is validated against the registry at runtime, which is why the schema
  uses a name pattern rather than a closed enum — a user agent has to be nameable there.

## Unified image build

The build context is materialized into the run directory from embedded assets plus the enabled
agents' install scripts. Phase order in `assets/agents/Dockerfile`:

1. **Base image** — `ubuntu:24.04` by default, overridable via `container.baseImage` (must stay
   Ubuntu 24.04-based: the Dockerfile uses apt and removes the default `ubuntu` user).
2. **Remove the default `ubuntu` user** — its UID (1000) is usually the one the sandbox user
   needs.
3. **`CACHEBUST`** — everything after this re-runs on `-r`, so package and CLI versions refresh.
4. **Base dependencies** (curl, git, iproute2, jq, nano, vim, ripgrep, ca-certificates, gnupg,
   sudo), then `EXTRA_PACKAGES` from `dependencies`, then the Docker CLI with its compose and
   buildx plugins.
   `iproute2` is required by the entrypoint's route injection.
5. **Root-phase agent installs** — every `agent-installs/<agent>/install-root.sh`.
6. **Sandbox user creation** — `AGENT_USERNAME`/`AGENT_HOME`/`UID`/`GID` mirror the host user.
   `useradd -l` avoids a huge sparse lastlog entry when the host UID is very high (WSL).
   Passwordless sudo.
7. **User-phase agent installs**, with `BASH_ENV=~/.bash_env` set up so nvm-installed tools work
   in the non-interactive shells agents use.
8. **Setup hooks** — `hooks.setup` scripts, copied in as `NNN-name.sh` and run in order.
9. **Entrypoint** — `assets/agents/entrypoint.sh`: point the default route at the gateway, run
   any mounted `/tmp/prestart-scripts/*` in order, then `exec` the agent command.

## Container user identity

The container user mirrors the host: username from `$USER`, home path from `$HOME`, and on Linux
also UID/GID. Docker Desktop and OrbStack remap IDs themselves, so passing host IDs there would
break the user — hence Linux-only.

Because the home path matches the host's, agent config paths look identical inside and outside
the sandbox, which is what makes `"~/.claude": "~/.claude"` inclusions work. The home directory is
baked into the image (CLIs, `.bashrc`, pre-seeded config) and is **not** persisted across runs:
persistent state must be brought in with `files.include`.

## The test agent

Because user agents are first-class, the e2e suite registers a trivial agent under a temporary
`HOME` whose command is a shell one-liner. Nothing in the test suite needs a real agent CLI or an
API key. See [recipes](recipes.md#run-the-tests).
