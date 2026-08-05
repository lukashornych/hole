# Golang Rewrite

We want to rewrite the entire Hole application in Golang. This will involve refactoring the existing codebase to use 
Golang's syntax and idioms, as well as potentially redesigning some components to take advantage of Golang's concurrency
model, builtin libraries and other features. The goal is to improve the maintainability of the application as well
as reduction of runtime dependencies for the end-user.

The app will still act as an orchestrator for Docker and Docker Compose, the user-facing API (CLI commands and settings file format)
should stay the same (except for the new features below).

## Tech stack requirements

- entire app in Golang
- no external dependencies besides Docker and docker compose (if possible)
- take advantage of Golang's concurrency model and builtin Docker SDK
- use Golang libraries instead of external dependencies whenever possible (jv, jq, etc.)
- automated testing (used by agent when adding new features as well as CI/CD pipeline for verification)
- ready to be released as binaries for WSL, Linux and macOS systems via the Github CI/CD pipeline

## Maintainability requirements

- use Golang's standard library and third-party libraries whenever possible
- follow Golang's best practices and coding conventions
- write clean, modular, and well-documented code (each features should be separately reviewable)
- use version control and automated testing to ensure code quality and maintainability

## Features

Most of the existing features will be in some form used, plus there will be new features that were still in the backlog. 
The existing edge-case exceptions in the existing codes must be reviewed and, if applicable, reimplemented in the new app.

### Installation process

The installation and uninstallation process should take care of the docker resources cleanup. When the app is installed,
it should remove all docker resources created by the app (e.g., previous versions, like if we upgrade from the actual version
to the Golang version). The uninstallation process should also any docker resources created by the app so they don't get
stale for the user to clean up manually.

### Docker management

The app should manage the docker compose files where possible instead of managing all of it manually using Docker SDK.

#### Why

`detect_container_runtime` (hole.sh:63) already hard-fails without a working `<runtime> compose`, and README:165 documents the plugin as a prerequisite. **The compose plugin is dependency #1 today.** So B's headline pitch — "removes the compose CLI dependency" — doesn't actually cash out:

- **A** keeps: docker/podman + compose plugin. Deletes `jq`, `jv`, `curl`, `tar`.
- **B** needs: docker/podman + *either* BuildKit (which ships as the CLI/buildx plugin anyway → nothing removed) *or* the Engine `/build` classic builder (the deprecated path Docker is retiring). Net dependency count doesn't improve, and you'd be building a fresh rewrite on a sunsetting API.

##### Side by side

|                                    | **A — shell out to `docker compose`**                        | **B — Docker Engine API direct**                             |
| ---------------------------------- | ------------------------------------------------------------ | ------------------------------------------------------------ |
| **Install deps**                   | unchanged (compose plugin, already required)                 | same or worse — see below                                    |
| **Image build**                    | BuildKit via compose, as today. Your Dockerfiles use no BuildKit-only features, but you keep cache parity | Engine `/build` = classic builder (deprecated), or shell out to `buildx` anyway |
| **Podman**                         | `podman compose` works out of the box today                  | Docker-compat socket. On rootless Linux that's a new `systemctl --user enable --now podman.socket` step for users — a *new* dependency. Or a second code path via `containers/podman/pkg/bindings` |
| **`docker attach` (hole.sh:1579)** | free — raw mode, signal forwarding, SIGWINCH→resize all handled by the CLI | **you hand-roll it**: hijacked stream, `term.MakeRaw`/restore, resize API on window change, Ctrl-C proxying, across macOS/Linux/WSL terminals. This *is* the product — if attach is janky the tool feels broken |
| **Healthcheck gating**             | `depends_on: condition: service_healthy`                     | ~30 lines polling `/containers/{id}/json` → `.State.Health.Status`. Healthchecks are an Engine feature, so this is the one genuinely easy part |
| **Teardown**                       | `compose down --remove-orphans` (hole.sh:1346) as catch-all + `label=com.docker.compose.project` filters (hole.sh:1162) | both primitives gone. You own the label scheme (arguably cleaner) but re-implement "remove everything in this project" — and CLAUDE.md says cleanup must never abort. That's exactly where `47e3be0` and `b3507e7` live |
| **`--with-docker`**                | privileged `docker:dind` sidecar, healthcheck, external volume — very compose-shaped | the most painful piece to port                               |
| **The 480-line YAML generator**    | **deleted either way** (see below)                           | deleted                                                      |
| **Code size**                      | smaller                                                      | larger, concentrated in the riskiest areas                   |

##### The thing you actually want is orthogonal

Both options delete `generate_instance_compose()`. Under A you replace the 480 lines of `echo` with typed structs → YAML. That's the real win and it doesn't require B.

Concretely: **hand-rolled structs with `yaml.v3` tags covering only the ~20 compose fields hole uses** (`build.args`, `image`, `pull_policy`, `volumes`, `environment`, `depends_on`, `networks`, `dns`, `extra_hosts`, `healthcheck`, `privileged`, `entrypoint`, `command`, `working_dir`, `stdin_open`, `tty`). Small, readable, zero dependency.

You *could* use `compose-go`'s `types.Project` instead — but verify it round-trips before committing. It's built as a loader; several fields have custom unmarshallers and don't necessarily marshal back to valid compose. Test: build a project with `build.args` + `depends_on: {condition: service_healthy}` + `extra_hosts` + external networks, marshal it, pipe to `docker compose config`. If it's awkward, hand-rolled structs are better anyway for your "minimal files I can verify" goal.

#### Recommendation: A

Note how much you've *already* left compose behind — networks are `external: true`, pre-created with your own subnet allocator; the DinD volume is `external: true`, created and seeded via raw CLI; startup is four sequential single-service `up -d` calls; attach and GC are raw `docker`. Compose is now doing four things: multi-file merge, BuildKit builds, container creation, and healthcheck-gated startup. B buys back the one you're deleting anyway (merge) and makes you re-implement the three that work.

Confine every runtime invocation to **one package** — `internal/engine`. Not an abstraction layer with interfaces; just *all the call sites live in one file*. If Docker ever ships a stable BuildKit-capable API, B becomes a contained rewrite of that one package. That costs nothing today, and it's the honest version of keeping the door open — don't build a pluggable backend now.

### Running sandboxes management

Unlike the previous version, where the app just spawned new sandboxes, now we want to be able to keep track of all the
running sandboxes and manage them. This will allow the user to easily see which sandboxes are running and manage them
(deleting, verifying some metadata, etc.).

### Logging

The app should have detailed logging for the user to monitor and troubleshoot issues as well as to know what is happening in the app.
It should also automatically log to a file for debugging purposes (use go best practices for it).

### Resources cleanup

The current cleanup process is not very reliable. We need to create process which will reliably clean up all of the docker
resources after the sandbox is exited (containers, networks). We could maybe leverage the new `Running sandboxes management`
to better track the resources and install more reliable exit hooks. Maybe the Go app could have a minimal background process
to track the exits instead of relying on the Linux exit states? After sandbox exit, there CANNOT be lest any container 
or network for that sandbox!

### Networking

The networking MUST support the following features:

- network allocation
    - the hole app must allocate docker network address space smartly, it should allocate as small address space as possible and should allow several parallel sandboxes (at least 20)   
- network isolation 
    - the agent cannot access internet by default, only if specifically allowed
    - the configuration must allow any port forward (any application protocol, not just HTTP and HTTPS) and must support domain filtering
- docker network name propagation - the name should be propagated to the sandbox container as env variable, some programs need it

### File management

The new version must support the same file inclusion and exclusion as the current version.

### Library management

The new version must support the same library functionality as the current version.

### Dependencies

The new version must support listing Ubuntu dependencies as the current version.

### Container settings

The new version must support the same container settings as the current version.

### Environment variables

The new version must support the same environment variable logic as the current version. 

### Configuration

The configuration should remain the same expect for the following changes:

- setup hooks should now support array of scripts that would be merged toggether with project specific settings
- the merge should have the same logic as host setup scripts
- the setup hooks should also support finding the scripts by pattern instead of defining specific files only (e.g., if we want to create multiple chunked scripts - 001-setup-npm.sh, 002-setup-maven.sh)

#### Profiles (NEW)

Today, a project has one fixed sandbox configuration, but the work done on it is not monolithic — different tasks require different agent configurations (different mounts, instructions, CLI flags) or different build/tooling dependencies (extra packages, Docker-in-Docker). Because switching means manually editing `settings.json`, in practice, people permanently run with the broadest variant ("everything allowed just in case"), which defeats the purpose of the sandbox. Additionally, there is no way to have multiple named working modes for a single project.

Profiles solve both issues: modes are defined once (versioned and shared in the repo), and the correct one is selected with a single argument at startup. The base remains minimal, and the agent receives broader permissions only upon explicit request by a profile.

##### What it is

Named sets of settings defined under a new `profiles` key in the existing `settings.json` (both global and project-level). A profile can contain the same keys as the root settings (`files`, `network`, `container`, `dependencies`, `environment`, `hooks`, `libraries`, `agents`) and is merged into the default configuration at startup.

##### Profile Selection

`hole start claude:research .` — without a profile (`hole start claude .`), the behavior does not change at all. A non-existent profile results in a startup error (it will print the available profiles).

##### Merge Behavior

Order (latest wins): global base → global profile → project base → project profile. Anything in the project file always overwrites global values. Arrays are concatenated, objects are deeply merged, scalars are overwritten — just like today.

Profiles are **purely additive**: they can only add permissions, never restrict them. The recommended pattern is a minimal base + profiles adding only what the given mode needs — the result is always a readable "base + chosen profile".

Newly, it is possible to define default CLI arguments for the agent in the settings (even within a profile) via `agents.<agent>.args`; command-line arguments (`-- <arguments>`) are appended after them and have the final word.

##### Profile Inheritance

A profile can inherit from one or more other profiles via `"extends"`. Orthogonal aspects (network, tooling, agent configuration) can thus be defined once and composed in the configuration — exactly one profile is always selected at startup, with no listing on the command line. Parents are applied before the child in the specified order, and the child has the final word; inheritance works even across global and project files; each profile is applied only once. A cycle or a reference to a non-existent profile = startup error.

### Sandbox image

The sandbox image with configured dependencies and setup hooks should be reusable more intelligently than now per project (this wastes a lot of disk space).
The app should create global or per-profile images to reuse for every sandbox that doesn't declare per-project dependencies or setup hooks.

### DinD

We need to support Docker-in-Docker (DinD) for running sandboxes in a containerized environment. But the current image
cache is suboptimal. 

The proposed approach is pull-through registry. The host would have long-running registry which each DinD would get passed to as `--registry-mirror`

### Git workspaces

We need special support for projects with Git workspaces.

#### Creating sandbox in workspace

If sandbox is created in a Git workspace, the Hole app should automatically link the main repo as library (RO by default, allow opt-in RW as global settings flag).

#### Creating sandbox in main repo

If the main repo has workspaces that are not nested in the same root directory, the Hole app should link the workspaces as libraries (RO by default, allow opt-in RW as global settings flag).

### Agents

Should support Claude Code, Gemini and Codex. It should also support the ability to add custom new agents by the user without
modifying the source code. There should be documented home folder where the agents can be configured and would be automatically registered by the app. Something like `~/.hole/agents/my-agent/...` which would be accessible by `hole start my-agent .` automatically.



### CLI usage

The CLI should remain the same as now (structure, parameters, parameter passing to agent, etc.).

I also want new parameters:

- `--library` to mount adhoc libraries on top of the settings (there can be multiple parameters to add mutliple libraries)

Also, there is currently bug that allows starting the sandbox without specifying project path (`.`) which shouldn't be possible in the new version.

Next i want simple commands for management of the sandboxes. Something like `hole list` which would show all  running sandboxes with metadata like - which settings files it uses, agent used, path to project, etc.
