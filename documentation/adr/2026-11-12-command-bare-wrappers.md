---
title: Expose npm-installed agent CLIs by bare name through a baked wrapper declared in command-bare.json
date: 2026-11-12
updated: 2026-11-12 09:20
status: accepted
kind: feature
issues: [67]
prs: []
areas: [internal/agents, internal/sandbox, assets/agents]
supersedes: []
superseded-by: []
relates: []
---

# Expose npm-installed agent CLIs by bare name through a baked wrapper declared in command-bare.json

An agent plugin may now carry an optional `command-bare.json`: the CLI's argv without its yolo
flags. Hole renders it into a POSIX sh wrapper (`exec '<part>' … "$@"`) and the image build
installs it as `~/.local/bin/<agent name>`. The builtin codex and gemini have one, so inside the
sandbox `codex` and `gemini` are callable by name, from a shell and from a shell-less exec (a
Claude Code plugin, an MCP server, a hook), always under the Node they are pinned to.

## Why

codex and gemini are npm CLIs installed under a pinned nvm Node (`v22.22.2`), which is not on the
image `PATH` (`~/.local/bin:/usr/local/...`). `command.json` reaches them by absolute path, but
nothing else did. In a sandbox where a setup hook installed Node 24 and nvm made it the default,
neither `bash -c` nor `sh -c` found `codex` or `gemini` at all, so anything that spawns the CLI by
name failed. The npm bins are symlinks to JS files with `#!/usr/bin/env node`, so the interpreter is
whatever `node` comes first — which is the constraint every option has to answer.

### Previous state

The pinned Node's `bin` was reachable only by its absolute path from `command.json`,
`version.json` and the update scripts. Bare-name use depended on nvm being loaded by bash's
`BASH_ENV` and on nvm's default happening to be the pinned Node.

## Options considered

### Option A — declarative `command-bare.json`, rendered into a wrapper (chosen)

- **Pros:** works in every exec context, independent of nvm's default; the pin lives in a file
  `TestPinnedNodeVersionMatchesTheInstallScript` already checks; Hole owns the quoting, so a user
  agent cannot get it wrong; optional, so existing agents are unchanged.
- **Cons:** one more file that carries the Node pin (five in total); the command is always named
  after the agent directory.

### Option B — a bash alias (declined)

- **Rejected because:** an alias exists only in interactive bash; a hook, MCP server or plugin
  execs without one.

### Option C — a symlink to the npm bin in `~/.local/bin` (declined)

- **Pros:** no generated file.
- **Rejected because:** the target's `#!/usr/bin/env node` picks the first `node` on `PATH` —
  none in `sh -c` or a shell-less exec, nvm's default in bash — not the pinned one.

### Option D — a convention in each agent's `install-user.sh` (declined)

- **Pros:** no new plugin file.
- **Rejected because:** the pin in a hand-written script is not covered by the pin test, and every
  user agent would write its own shell quoting.

### Option E — derive the wrapper from `command.json` (declined)

- **Rejected because:** the yolo flags cannot be told apart from the command; the wrapper must pass
  only the user's arguments.

### Option F — expand host variables like `command.json` does (declined)

- **Rejected because:** the wrapper is baked into the image, and a host variable's value would get
  into it without changing the image hash. Only `$HOME`/`${HOME}` are replaced, by the sandbox home,
  which is already part of the image identity (`HostIdentity.Home`).

### Option G — put the pinned Node's `node`/`npm`/`npx` on `PATH` globally (declined)

- **Rejected because:** on a shell-less exec it would shadow the Node a setup hook installed on
  purpose, and it goes beyond exposing the agent CLIs.

## Decision

**Chosen: Option A.** Like Option D it runs the pinned interpreter in every exec context; it wins
because the pin stays under `TestPinnedNodeVersionMatchesTheInstallScript` and the quoting is
Hole's, not each user agent's. Revisit the naming if a user agent needs a CLI named differently
from its directory: an object form `{"name": …, "command": […]}` would lift that limit.

## Key technical details

- `internal/agents/agents.go` — `BareCommand()`: missing file is `nil`; invalid JSON, `[]`, an
  empty item, or a first item without `/` is an error. A bare first item is resolved through
  `PATH`, which the wrapper leads — `["my-agent"]` would exec itself forever. This also rules out
  an `env X=1 …` prefix; added during review, beyond the original file contract.
- `internal/sandbox/start.go` — `renderBareWrapper` and `materializeBuildContext` write
  `agent-installs/<agent>/bare-command` (no `.sh` suffix, so the install loops never run it).
- `assets/agents/Dockerfile` — the `for wrapper in` loop runs as the sandbox user **after** the
  user-phase installs and **before** the setup hooks, so a hook can replace a wrapper deliberately.
  An existing target (file or symlink, dangling included) fails the build: a silent skip would leave
  a bare name running something other than the file declares. Claude has no `command-bare.json`
  because its native installer already puts `claude` there and the build would fail.
- The file is not update-only, so editing it rebuilds the image (`BuildInputsHash`,
  `hashAgentDir`). `Dockerfile.update` does not touch the wrapper; it stays valid because updates
  keep the pinned Node path.
- **Looks like a bug, is not:** in bash, `BASH_ENV` loads `nvm.sh`, which puts nvm's default
  Node's `bin` ahead of `~/.local/bin`. A setup hook that installs its own `codex` there wins in
  bash; a shell-less exec always gets the wrapper.

## Verification

- Unit: `TestBareCommand`, `TestBuiltinBareCommands`, `TestPinnedNodeVersionMatchesTheInstallScript`
  (now includes `command-bare.json`), `TestRenderBareWrapper`,
  `TestBareWrapperPassesArgumentsAndExitCode` (runs the wrapper through `sh`: spaces, quotes, empty
  arguments, exit code), `TestMaterializeBuildContextWritesBareWrappers`,
  `TestBareWrapperLoopInstallsWrappers`, `TestBareWrapperLoopFailsOnAnExistingTarget`.
- Integration: `TestBareWrapperIsCallableByNameInABuiltImage` — a real build of the Dockerfile
  loop on alpine:3.19 with a non-default `AGENT_HOME`, then the agent run by bare name as the
  container entrypoint (no shell). Passed on docker 29.7.1 (linux daemon, reached over
  `DOCKER_HOST` from the agent sandbox).
- `make itest`: every package passed except `internal/sandbox`, which ran past the Makefile's
  per-package `-timeout 20m` without a failing test. Rerun alone with `-timeout 50m` it passed in
  1209 s; the new integration test takes ~19 s of that (a no-cache build on a remote daemon), so the
  package sat just under the limit before this change.
- e2e on the host (codex/gemini by name in bash and `sh -c`, and under a setup hook that makes
  another Node nvm's default) was not run when this record was written.

## Consequences & open follow-ups

- The builtin `command.json` and `version.json` could call `$HOME/.local/bin/<agent>` instead of
  the full nvm path, leaving the pin in three files instead of five. Deferred: a separate change,
  and the e2e test agent has to be checked against it.
- Expected from reading the code, **not reproduced**: `materializeBuildContext` writes the build
  context with the host umask, and the main Dockerfile does not `chmod a+rX /tmp/agent-installs`
  (`Dockerfile.update` does for its own directory). Under umask 077 the sandbox user would not be
  able to traverse the root-owned agent directories, so the wrapper glob would stay unexpanded and
  the build would be green without wrappers. The `install-user.sh` loop looks exposed the same way;
  this predates the change and was left alone.

## Timeline

- **2026-11-12** — implemented on `feat/command-bare` (issue #67).
