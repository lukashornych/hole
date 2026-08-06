# Architecture

Hole is a single Go binary (`cmd/hole`) that orchestrates a multi-container sandbox via Docker
Compose (or Podman Compose). There is no daemon and no server: `hole start` builds and starts
the sandbox, attaches the terminal to the agent container, and a detached supervisor destroys
everything when the agent exits.

## Package layout

```
cmd/hole/              thin entry point: logging setup, dispatch to internal/cli
assets/                go:embed root — agent plugins, Dockerfiles, entrypoints, schema
internal/cli/          argument parsing, help text, command dispatch
internal/config/       settings load/validate/merge, profiles, exclusion glob matcher
internal/hostenv/      env expansion, path pipeline, host identity, ~/.hole layout, identity
internal/engine/       the only package that shells out to docker/podman
internal/compose/      typed compose model (~20 fields) + YAML marshalling
internal/network/      allow-list model, gateway artifact generation, subnet allocator
internal/agents/       agent registry: embedded builtins + ~/.hole/agents user agents
internal/image/        canonical image config, manifest hashing, image identity and scope
internal/hooks/        hook script resolution and host-side execution
internal/sandbox/      orchestration: startup, mounts, compose generation, teardown, dump,
                       the watchdog handoff, garbage collection, `hole list`
internal/state/        instance registry under ~/.hole/instances
internal/trust/        per-project consent for host-affecting project settings
internal/worktree/     git worktree detection and the libraries it implies
internal/dindregistry/ the long-lived pull-through image cache
internal/update/       release discovery, self-update, version-change migration, uninstall
internal/logging/      slog setup: console handler + per-run JSON file handler
internal/version/      version stamping
test/e2e/              end-to-end suite driven by a generated test agent
```

`internal/engine` deliberately has no interface or abstraction layer. Its value is that every
runtime invocation lives in one file, so podman quirks and missing prune filters have exactly
one place to be handled.

## Assets

Every runtime file — Dockerfiles, container entrypoints, agent plugins, the JSON schema — lives
under `assets/` and is embedded with `go:embed`. The binary is the package; there is no install
directory to keep in sync. See [guidelines](guidelines.md#non-negotiable-rules).

## Sandbox identity

- **Project name**: sanitized basename + `-` + the first 8 hex characters of the sha1 of the
  absolute path. Stable per project, and two projects with the same basename never collide. Used
  for image repositories and `hole destroy <path>`. The hash covers the path **as written**, not
  the sanitized form: sanitizing loses `_` and case, and `hole destroy` force-removes by this
  prefix — so a collision would tear down a live sandbox of a different project.
- **Instance ID**: 6 random `[a-z0-9]` characters from `crypto/rand`.
- **Instance name**: `hole-sandbox-<project_name>-<instance_id>` — the compose project name, the
  network name prefix, and the container name prefix.

## Directories

| Path | Contents |
|---|---|
| `~/.hole/settings.json` | global settings |
| `~/.hole/agents/<name>/` | user-defined agents |
| `~/.hole/instances/<instance>.json` | one file per running sandbox (plus its lock files) |
| `~/.hole/logs/run-*.log` | per-run debug logs, ~7 day retention |
| `~/.hole/tmp/run.XXXXXX/` | per-run generated artifacts |
| `~/.hole/state.json` | the last version that completed a run |
| `~/.hole/trust.json` | project paths whose settings the user accepted, and what they accepted |

The run temp directory lives under `$HOME` rather than `$TMPDIR` because Colima, Lima and
Podman-Machine VMs share `$HOME` but not `/var/folders`, and generated files there must be
bind-mountable into containers.

## Container architecture

Two networks per instance, both created by Hole and referenced as `external` in the generated
compose file:

- `<instance>_sandbox` — `internal: true`, where the agent runs. The gateway holds the fixed
  address `<subnet>.53` on it and is the sandbox's DNS server.
- `<instance>_internet` — a plain bridge the gateway masquerades out of.

Services:

- `gateway` — DNS policy, router and firewall in one container. See [networking](networking.md).
- `agent` — the unified agent container with every enabled agent CLI installed.
  See [agents](agents.md).
- `docker` (optional) — a `docker:dind-rootless` sidecar (privileged container, unprivileged
  daemon). See [configuration](configuration.md#docker-in-docker).

## Startup sequence

1. Resolve host identity, project name and instance ID; create the run temp directory.
2. Write the instance state file **before any Docker resource exists**, take the run's liveness
   lock, and spawn the teardown watchdog. From here on, an abort anywhere has an owner.
3. Validate and merge the settings documents (with the selected profile's chain, if any); resolve
   the agent registry and verify the startup agent is enabled.
4. Resolve `cleanupHost` hooks and snapshot the merged settings into the state file, so teardown
   works even if the next step aborts.
5. Build the egress policy, resolve the image identity, run `setupHost` hooks.
6. Run the version-change migration and garbage collection.
7. Allocate and create the two networks, then the DinD volume — in that order, so a half-started
   instance is always recognisable by its network.
8. Materialize the build contexts and gateway configuration; generate one compose file.
9. `compose up -d` per service: gateway → docker (if enabled) → agent. Health gating comes from
   compose `depends_on: condition: service_healthy`.
10. `docker attach` the agent container. Raw mode, terminal resize and Ctrl-C proxying stay the
    runtime CLI's problem, which is the main reason Hole shells out rather than using the Engine
    API.

    Whether the CLI has a terminal decides two things together, and they only work as a pair. With
    one, the agent service gets `tty` and `stdin_open` and the attach forwards stdin. Without one —
    a pipe, a CI job, the e2e suite — the service gets neither and the attach passes `--no-stdin`.
    Dropping only the stdin forwarding is not enough: `stdin_open` keeps the agent's process alive
    waiting for input that can no longer arrive, which hangs an interactive command (notably the
    `-d` debug shell) forever. With no TTY and no open stdin it reads EOF and exits, while output
    still streams and the exit code still propagates.

    The terminal test is a real ioctl (`engine.IsTerminal`), not a character-device check:
    `/dev/null` is a character device, and `/dev/null` is exactly what a non-interactive run gets
    for stdin.
11. After the agent exits, mirror the watchdog's teardown progress until the instance is gone.

The exit code the CLI reports comes from the agent *container*, not from the attach client,
whenever the container has already stopped. An agent whose command finishes before the attach
lands makes the runtime refuse with "cannot attach to a stopped container" and exit 1, which would
otherwise turn a successful short-lived agent into a failed run.

## Reliability: registry, watchdog, GC

### Instance registry

`~/.hole/instances/<instance>.json` records everything teardown, `hole list` and GC need:
identity, flags, the merged settings snapshot, both process IDs, networks and subnets, the DinD
volume, the run directory and log file, and the start time. Writes are atomic (temp file +
rename) so a reader never sees half a file.

Docker labels (`hole.managed`, `hole.instance`, `hole.project`) remain the ground truth for what
exists; the state file is the metadata cache. Its real job is letting GC distinguish an
**abandoned** instance from a healthy concurrent one — a distinction the bash implementation
could not make, which is why `kill -9` orphans were unrecoverable there.

Liveness is a **file lock**, not a PID check: the CLI holds an exclusive `flock` for its whole
run, and the kernel releases it when the process exits — before the parent reaps it. A process
that has exited but not been reaped still answers signal 0, so a PID check would call a dead CLI
alive for as long as its zombie lingers.

### Watchdog

Right after the state file is written, the CLI spawns `hole __watchdog <instance>` detached
(`setsid`, stdio appended to the run log). The watchdog — not the CLI — performs teardown in
**every** runtime case. Two things follow: the cleanup path is single-owner and continuously
exercised (the code that runs after `kill -9` is the code that runs on every clean exit), and
teardown is immune to terminal lifecycle, because signals cannot reach a setsid'd process.

Watchdog logic: until the agent container has **started**, watch the CLI's liveness lock — a
startup that aborts before any container must still have its partial resources removed. Once it
has started, `docker wait` it, but keep watching the lock and the abort marker alongside: tear
down when the container stops, when the CLI asks, or when the CLI dies, whichever comes first. The
CLI drops the abort marker on early failure so the common failure case has no polling lag. (A
marker rather than a signal: a signal sent in the milliseconds before the watchdog installs its
handler is lost, or worse, kills it.)

**The liveness lock has to be polled in the second phase too**, not just the first. A SIGKILLed
CLI runs no cleanup of its own, so waiting only on the container means relying on the CLI's death
stopping it — which happens by terminal hangup, and therefore only for a TTY-enabled container.
A non-interactive run allocates no TTY (see step 10), so the sandbox would simply outlive its
owner; an agent that ignores the hangup would do the same even with a TTY. The kernel releases the
lock when the CLI dies, so no grace period is needed, and a clean run cannot trip it: the CLI's
defers release the lock only *after* teardown has finished.

**Started, not merely created**, and the distinction is load-bearing: compose creates the agent
container and starts it only once the gateway reports healthy, and `docker wait` on a created
container returns 0 immediately — indistinguishable from a clean exit. Reading existence as
"running" therefore tears the sandbox down while it is still starting, and the visible symptom is
compose failing with `No such container` against a container it had created moments earlier.
`engine.ContainerStarted` is the predicate; an exited container still counts as started, or a
container that stops between two polls would never be noticed.

Because the CLI stops relaying when the instance leaves the registry, **deregistering is the last
thing teardown does** — after the completeness check and the closing message. Anything logged
after `store.Remove` reaches the log file but races the CLI's exit, so it may never appear on the
console; that silently swallowed both the "Sandbox destroyed" line and, worse, the warnings naming
resources teardown could not remove.

CLI side, after attach returns or startup fails: mirror the watchdog's progress by tailing the
run log — its records carry `component=watchdog`, which is how the CLI relays exactly those and
none of its own — until the state file disappears. The prompt therefore returns only once the
resources are gone, so an immediate re-start cannot race the previous sandbox's cleanup. If the
watchdog is dead or stalls past a timeout, the CLI runs the same shared teardown itself.

### Teardown

One shared function (`sandbox.Teardown`) drives all three callers: watchdog, CLI fallback, and GC
for abandoned instances. It is guarded by a per-instance `flock` — a reentrancy guard, not a
coordination mechanism; whoever loses the race finds the instance already deregistered — and
works entirely from the state file, which it re-reads after locking, because a supervisor that
started earlier holds a snapshot with no networks in it.

Phases: `-n` dump → `compose down --remove-orphans` (fileless, `-p` only, so teardown never
depends on generated files) → registry mirror detach → explicit removal of both networks with an
attached-container force fallback → DinD volume → `cleanupHost` hooks → run directory → state
file → a final verification pass that names any leftover and prints the command to remove it.

The force fallback exempts the registry mirror: it is shared by every sandbox, and the fallback
is reached exactly when the recorded detach did not happen (the CLI killed between `Attach` and
the `store.Write` that records it, or a failed `Detach`). It is disconnected instead, which frees
the network without destroying another sandbox's image cache.

**Nothing in teardown is gated on how far startup got.** That was the root cause behind the bash
version's "cleanup seems random" symptom: `compose down` only ran when the final `up -d agent`
had succeeded, so an earlier failure leaked containers, and the network removal then failed
invisibly because containers were still attached.

Signals (INT/TERM/HUP) reach the CLI for the whole run, including the image build: they stop the
agent container, which ends an active attach, let the interrupted step return, and set the exit
code to the conventional 130/143/129.

### Garbage collection

GC runs on every `start` and `list`. It is the backstop for the one case the watchdog cannot
cover — CLI and watchdog killed at once — and it is deliberately conservative, because anything
it removes might belong to a concurrent start:

| Pass | Rule |
|---|---|
| Abandoned instances | CLI lock free **and** watchdog PID dead → full teardown, including running containers |
| Networks | runtime prune filtered by `hole.managed` **and** `until=10m`, so a start that created its networks but no containers yet is safe |
| Containers | stopped sandbox containers whose instance has no state file, no running sibling and **no networks left** (compose has a window where networks exist but containers do not) |
| Volumes | per-instance DinD volumes whose instance has neither network nor container — volume prune has no age filter, so sibling liveness is the age proxy |
| Run directories | `~/.hole/tmp/run.*` older than a day that no registered instance owns |
| Run logs | older than 7 days |

Image GC is separate and runs only *after* the agent service is up — never before, or a failed
build could have destroyed the last working image. See
[configuration](configuration.md#image-identity-and-scope).

## Destroy

- `hole destroy <path>` — containers, networks, that project's own image repository, and DinD
  volumes for one project. The shared agent image and the gateway image are preserved; they may
  serve other projects.
- `hole destroy` — everything Hole owns, including the registry mirror.

## Security model

**Network isolation**

- The agent container sits on an `internal: true` network. Its default route points at the
  gateway, which is the only path off that network.
- The gateway denies by default and filters at L3/L4, so every protocol and port is covered and
  no tool needs proxy awareness.
- A name that is not allowed does not resolve; an address is only reachable if the sandbox's own
  resolver handed it out for an allowed entry.

**File access control**

- The project is mounted read-write at the **same absolute path** as on the host, and is the
  container working directory.
- Secrets are hidden by over-mounting: files get `/dev/null`, directories get an empty host
  directory. Never anonymous volumes — `compose down` leaks those without `-v`.
- `files.include` and `libraries` are opt-in; libraries are read-only by default.

**The agent runs as a non-root user** mirroring the host user, so files it creates in the project
have the right ownership. It does have passwordless `sudo` inside the container: the container is
the boundary, not the user. `NET_ADMIN` on the agent is likewise safe — the only route out is
through the gateway, so rewriting routes inside the agent can break its own connectivity but
never widen it.

**Project settings are untrusted input.** `<project>/.hole/settings.json` is repository content,
and a few of its keys reach past the sandbox: `hooks.setupHost`/`cleanupHost` run scripts as the
invoking user, `files.include`/`libraries` mount host paths, `container.docker` adds the privileged
sidecar, `network.hostGatewayDomains` reaches services on the developer's machine,
`hooks.setup`/`dependencies` run during a build that uses the host's unfiltered network, and
`network.allow` widens egress. Those need a per-project confirmation, recorded in
`~/.hole/trust.json`; the gate sits before the settings snapshot is written, because teardown
replays `cleanupHost` from it. See
[configuration](configuration.md#project-trust).

**Docker-in-Docker is the weak point in this model, deliberately bounded.** The sidecar container
must be privileged (rootlesskit will not start otherwise), so it is the one place an agent-reachable
container holds the capabilities a container escape needs. The daemon inside it runs **rootless** so
the agent cannot use it to read the host — a nested `--privileged` container maps to a subuid that
owns none of the host's devices, verified against both the raw-disk read and the exclusion-strip
escapes ([analysis/security-audit.md](../analysis/security-audit.md), findings 1 and 3). What
remains is a kernel- or runtime-level escape from the privileged sidecar, which is why DinD is
off by default and documented as a larger surface than the rest of the sandbox. See
[configuration](configuration.md#docker-in-docker).
