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
  update-root.sh     # optional — runs as root during an in-place update (rare)
  update-user.sh     # optional — runs as the sandbox user during an in-place update
  version.json       # optional — how Hole reads the installed and the latest version
  command-bare.json  # optional — makes the CLI callable by the agent's name inside the sandbox
```

- `command.json` is emitted as the compose `command`. Entries may reference `$HOME`, which Hole
  expands to the sandbox home before writing the compose file — the gemini and codex commands
  use it to invoke a pinned nvm node binary. Commands run the agents in their
  "skip permissions / yolo" modes: the sandbox is the safety boundary, not the agent's own
  prompts.
- `allow.txt` uses the allow-list shorthand (`<host>[:<port>,...]`, default ports 443 and 80),
  with `#` comments. Entries from **every enabled agent** are merged regardless of which agent
  starts, so switching agents inside one image needs no settings change — and so every entry is
  part of the egress surface of every sandbox, whichever agent runs.
- Because of that merge, an entry must be **no wider than the CLI genuinely needs**, and must never
  wildcard a namespace the agent's vendor does not exclusively own. `*.chatgpt.com` is one vendor's
  subdomains; `*.googleapis.com` includes `storage.googleapis.com`, where anyone can create a
  writable bucket. The gateway matches DNS names and L3/L4, never URL paths, so a multi-tenant
  wildcard allows attacker-controlled endpoints indistinguishably from the vendor's own API. Name
  the specific hosts instead, and leave optional traffic (telemetry, alternative backends) out —
  users add those through `network.allow`.
- `install-user.sh` typically also pre-seeds config so the user is not re-prompted in every
  sandbox — claude writes `~/.claude.json` with the onboarding and trust flags accepted.
  `~/.claude.json` is baked into the image (only `~/.claude/` is mounted), so an answer given in
  one sandbox is lost in the next.
- Claude's workspace trust dialog is suppressed by `CLAUDE_CODE_SANDBOXED=1`, not by the baked
  `projects["/"]` trust entry: Claude Code stopped honouring trust entries above a project's git
  root (and above `$HOME` outside git), so that entry only still helps older CLIs and non-git
  directories outside `$HOME`. The variable is set twice, because the compose `command` is exec'd
  without a shell: `command.json` prefixes `env CLAUDE_CODE_SANDBOXED=1`, and `install-user.sh`
  exports it from `~/.bash_env` for `claude` started in a `-d` shell or a hook. It is not in Claude
  Code's public documentation; if a release renames it, the dialog returns but nothing is
  weakened. `.bash_env` is image-wide, so shells in gemini and codex sandboxes export it too —
  harmless, as only Claude reads it. `TestClaudeRunsAsSandboxed` guards both places.
- Every install and update script cleans its package manager's cache in the same script — npm's
  alone was ~300 MB of the image, and a cache written in one layer is never reclaimed by a later
  one.
- `command-bare.json` has the shape of `command.json` (a non-empty JSON array of non-empty
  strings) but holds the CLI **without** its yolo flags, and its first item must be a path
  (containing `/`): a bare name would be resolved through `PATH`, which the wrapper itself leads,
  so `["my-agent"]` would exec itself forever (an `env X=1 …` prefix is not possible for the same
  reason). Hole renders it into a POSIX sh wrapper,
  `exec '<part>' … "$@"` with every part single-quoted, and the build installs it as
  `~/.local/bin/<agent name>` — the first directory on the image `PATH`. It exists for CLIs that
  run under a pinned nvm Node (gemini, codex): that Node is not on `PATH`, and a symlink to the npm
  bin would run through `#!/usr/bin/env node`, i.e. under whatever Node comes first — none at all
  in `sh -c` or a shell-less exec (an MCP server, a hook), nvm's default in bash. An alias would
  exist only in interactive bash. Only `$HOME`/`${HOME}` are replaced (textually, as in
  `command.json`, by the sandbox home); every other `$` stays literal, because the wrapper is
  baked into the image and a host variable's value would get into it without changing the image
  hash. The command is always named after the agent
  directory. Claude has no such file: its native installer already puts `claude` into
  `~/.local/bin`, and a wrapper would collide with it.
  - The wrappers are installed **after** the user-phase installs and **before** the setup hooks. If
    `~/.local/bin/<agent>` already exists by then (a file or a symlink, dangling included), the
    build fails naming the agent and the path, rather than leaving a bare name that runs
    something other than the file declares. A setup hook may deliberately replace a wrapper.
  - In bash, `BASH_ENV` loads `nvm.sh`, which puts the `bin` of nvm's *default* Node ahead of
    `~/.local/bin`; a setup hook that installs its own `codex` under that Node therefore wins in
    bash. That is the hook's explicit choice and is left alone; a shell-less exec always gets the
    wrapper.
- `update-*.sh` and `version.json` opt the agent into [in-place updates](#in-place-agent-updates);
  an agent has both or neither.

> **The Node version is pinned in the files that name the interpreter, and they must agree.**
> gemini and codex name the interpreter by absolute path
> (`$HOME/.nvm/versions/node/v<x.y.z>/bin/node`) because the agent is `exec`'d without nvm's shell
> initialisation, so `install-user.sh` must run `nvm install <x.y.z>`
> for exactly that version. A floating `nvm install 22` drifts to a newer patch at build time and
> the agent then fails to launch with ENOENT — invisible until someone starts it. `version.json`
> runs the CLI the same way and carries the same path, as does `command-bare.json`. Changing the
> version means editing `install-user.sh`, `command.json`, `version.json`, `command-bare.json` and
> `update-user.sh`;
> `TestPinnedNodeVersionMatchesTheInstallScript` fails if they disagree. Update scripts never
> install another Node and do not load nvm: they run the pinned Node's npm by absolute path (and
> prepend its `bin` to `PATH` for that process only). nvm's default can be a different Node — a
> setup hook that installs one makes it the default — and its npm would update a CLI the agent never
> runs; the update is then a no-op and is offered on every start. Neither `nvm use` nor
> `nvm alias` is used, so the user's default stays whatever the setup hooks made it.

## In-place agent updates

`hole start --update-agents`, or a `Y` at the start's version prompt, updates agent CLIs inside the
existing image instead of rebuilding it. `-r` re-runs everything after `CACHEBUST` — apt,
`dependencies`, every installer and all `hooks.setup` scripts — which is slow and fails for reasons
unrelated to the agents.

**Agent set.** Only enabled agents are ever checked or updated: the image contains exactly
`container.enabledAgents`, so the set is the same list the build context uses.
`--update-agents` takes every enabled agent with an update script (and warns about the others); a
`Y` takes only the agents the prompt listed, so one agent's update adds no layer for the others.

**Plugin files.**

- `update-root.sh` / `update-user.sh` update the CLI with its own mechanism. Builtins: `claude
  update`, and `npm install -g <package>@latest && npm cache clean --force` for codex and gemini. An
  agent without either is skipped by `--update-agents` with a warning — the right behaviour for a user agent that pins
  its version in `install-user.sh`. Update scripts must not change Node, must clean their cache,
  and must leave configuration alone (pre-seeded `~/.claude.json`, whatever setup hooks did).
- `version.json`:

  ```json
  {
    "command": ["$HOME/.local/bin/claude", "--version"],
    "latest": { "url": "https://downloads.claude.ai/claude-code-releases/latest" }
  }
  ```

  `command` runs inside a container of the image, with `$HOME` expanded like `command.json`; it is a
  separate command because `command.json` carries flags (`--dangerously-skip-permissions`,
  `--yolo`) whose interplay with `--version` is not guaranteed. The installed version is the first
  `x.y.z` in its output. `latest` is exactly one of `{"npm": "<package>"}` (the package's `latest`
  dist-tag from `registry.npmjs.org`) or `{"url": "<http(s) URL>"}` (plain text or
  `{"version": "…"}`); a response that does not look like a version is discarded, so an HTML error
  page never becomes "latest". Claude's URL is what its own installer reads, so the check follows the
  same `latest` channel the image was built from.
- **Both or neither** (`Agent.CheckUpdateFiles`, enforced by `Registry.ResolveEnabled` for enabled
  agents only): an update script without `version.json` would be updated but never verified — and
  could not tell the no-op detection that it changed — while a `version.json` without an update
  script would offer an update that nothing can install. Either, or a malformed `version.json`,
  fails the start with an error naming the agent and the file; the registry load itself does not
  fail, so a broken agent that is not enabled stays harmless.

**Update build.** `assets/agents/Dockerfile.update` is built directly (`engine.ImageBuild`, not
compose) from a context in the run directory holding only the selected agents' update scripts:

1. The current image is inspected once more (`engine.ImageInspect`: ID, size and labels in one
   call, so the labels belong to exactly that ID) and its ID is tagged
   `hole-sandbox/agent-update:<instanceID>-base`, which becomes `FROM`. The start's own read is not
   reused here: the update prompt can block for minutes while a sibling start retags the reference.
   Tagging the ID pins exactly the image whose labels were read, and BuildKit does not accept an ID
   in `FROM` (it expands `sha256:…` to `docker.io/library/sha256:…` and tries to pull).
2. One `RUN` executes every `update-root.sh` with `HOME=/root` (the image's `ENV HOME` is the
   sandbox home, unlike during `install-root.sh`), then every `update-user.sh` through `runuser`
   with the sandbox user's `HOME`, `BASH_ENV` and `PATH`. With the `COPY` that is two layers per
   update; the base image has ~19 and overlay2 caps at ~127.
3. Labels `com.hole.clean-size` and `com.hole.agent-updates` are set, and `USER ${AGENT_USERNAME}`
   is the last instruction — without it every container of the updated image would run as root.
   `ENV`, `ENTRYPOINT` and `com.hole.image` are inherited.
4. The build always runs with `--no-cache`: after a no-op or a rejected update the base and the
   scripts are unchanged, and a cache hit would replay the old result instead of fetching a newer
   release. On docker it also passes `--provenance=false --sbom=false`, so the containerd image store
   produces a plain image rather than a manifest list with attestations.

The result is tagged `hole-sandbox/agent-update:<instanceID>` — a repository of its own, because
the image GC of a concurrent start removes every other tag of the agent repositories. Docker needs
a buildx builder with the `docker` driver, the only one that sees the daemon's image store; the
engine reads the current builder's driver (`docker buildx inspect`) and otherwise skips the update
with a warning naming the builder. It never switches builders: on a non-default context the
docker-driver builder is named after the context (`colima`), and other builders can be in an error
state. Podman's buildah always sees local storage.

**Validate, then retag.** Before the build the current image's versions are known (cache by image
ID, else one throwaway run of that ID — not of the reference, which a concurrent update or `-r` of
another start may retag meanwhile). After it, the `version.json` commands of **all** enabled agents run in
one throwaway container (`--network none`, entrypoint `bash`, one marker line per agent). Any agent
with a `version.json` that prints no version rejects the update — this catches an npm install of a
CLI that needs a newer Node, which npm only warns about. All versions identical to before is a
no-op: nothing is retagged and nothing counted (a filesystem diff cannot tell, since updaters write
logs and state even when nothing changed). Otherwise the real reference is retagged and the
validated versions are cached under the new image ID. The old image becomes dangling; a sandbox
still running it is unaffected, and the labelled dangling prune collects it once unused. The base
tag may be that image's last name while a sibling sandbox runs it, which plain `rmi` refuses, so
the temporary references are dropped with `engine.ImageUntag` (`rmi --force` on docker, which only
untags; `podman untag`, because podman's `rmi --force` would remove the containers). Every failure
is a warning and the start continues with the old image; a signal during the build ends the start.

**Growth guard.** Lower layers keep every old CLI, so each update grows the image (+22.7 % measured
for claude and codex together before the npm cache cleanup). Before an update, `.Size` divided by
`com.hole.clean-size` ≥ 2, or `com.hole.agent-updates` ≥ 40 (layer backstop), asks for a clean
rebuild; `Y` is exactly the `-r` path, never automatic, because `-r` re-runs hooks and needs the
network for every installer. `.Size` is only compared with itself: it is the unpacked size on
overlay2 and podman, the compressed content size on the containerd store. A clean build has neither
label, so the baseline resets on its own; adoption retags keep them. A no-op or rejected update removes its
image but its layers stay in the builder cache until `docker builder prune` — accepted, since such
updates are rare.

**Version check on start** (`internal/agentversion`). Runs when the image exists, without `-r` or
`--update-agents`, with `container.agentUpdateCheck` not `false`, and only for enabled agents with a
`version.json`. The start inspects the agent image once (after adoption, skipped on `-r`) and hands
that read to both the banner and the check, so a start without an update costs one `image inspect`. Installed versions come from `~/.hole/agent-versions.json`, keyed by image ID (a
miss costs one throwaway container, ~1–2 s once per image). A probe in which any agent prints no
version is warned about and not cached — a cached gap would hide that agent's updates for the
image's lifetime — so a permanently broken version command costs that container on every start
until it is fixed. Latest versions are fetched **on the
host with `net/http` only** — the host has no npm, node or curl, and no agent-provided program may
ever run on it — in parallel, each agent's fetch with its own 1 s timeout, at most once per 4 h per agent after a
successful fetch. A failed fetch is not recorded, so it is retried on the next start — an offline
host pays up to 1 s per start — while the last successful version stays known and keeps being
offered. Every checked agent has an update
script too (both or neither), so every newer version can be offered. `s` records the offered version as `skipped`; an agent is offered
again only when `GreaterThan(latest, skipped)` — not inequality, so a rolled-back dist-tag does not
re-offer an older release — and an update installing the skipped or a newer version clears it. The
state file is written through a unique temporary file and a rename, because concurrent starts share
it; entries of images that no longer exist are dropped on every write. A state unchanged since it
was loaded is neither pruned nor written — pruning costs one `image inspect` per cached image — so
most starts leave the file alone and stale entries wait for the next real change.

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
8. **Bare-name wrappers** — each `agent-installs/<agent>/bare-command` (rendered from
   `command-bare.json`) is installed as `~/.local/bin/<agent>`; an existing target fails the build.
9. **Setup hooks** — `hooks.setup` scripts, copied in as `NNN-name.sh` and run in order, each
   between `::hole-hook-begin`/`::hole-hook-end … exit=<rc>::` markers that Hole splits into
   per-script logs ([architecture](architecture.md#hook-logs)).
10. **Entrypoint** — `assets/agents/entrypoint.sh`: point the default route at the gateway, run
    any mounted `/tmp/prestart-scripts/*` in order (with the same markers; the first failing script
    ends the container with its exit code), then `exec` the agent command.

The build `SHELL` is bash with `pipefail` but **without `-e`**, so a `for` loop's status is only
that of its last iteration. Every loop over scripts (agent installs, bare-name wrappers, setup
hooks) therefore exits on a failing iteration itself (`|| exit $?`); without that, a failing
script followed by a succeeding one gives a green build with a broken image. `hookloops_test.go` runs the loops
straight out of the embedded Dockerfile to hold this.

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
