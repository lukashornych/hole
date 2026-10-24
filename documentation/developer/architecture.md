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
internal/config/       settings load/validate/merge, profiles, exclusion glob matcher,
                       settings scaffolding
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
internal/worktree/     git worktree detection: the libraries it implies and the worktree pool
internal/dindregistry/ the long-lived pull-through image cache
internal/agentversion/ agent version check: parsing, latest-version fetch, state file, prompt
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
- **Instance ID**: 6 random `[a-z0-9]` characters from `crypto/rand`, or the caller's choice with
  `hole start --instance-id` (same format, `hostenv.ValidateInstanceID`).
- **Instance name**: `hole-sandbox-<project_name>-<instance_id>` — the compose project name, the
  network name prefix, and the container name prefix.

## Directories

| Path | Contents |
|---|---|
| `~/.hole/settings.json` | global settings (replaced by `$HOLE_SETTINGS_FILE` when set; nothing else moves) |
| `~/.hole/agents/<name>/` | user-defined agents |
| `~/.hole/instances/<instance>.json` | one file per running sandbox (plus its lock files) |
| `~/.hole/logs/run-*.log` | per-run debug logs, ~7 day retention |
| `~/.hole/logs/run-*/` | per-run hook logs and `build.log` ([hook logs](#hook-logs)), same retention |
| `~/.hole/tmp/run.XXXXXX/` | per-run generated artifacts |
| `~/.hole/state.json` | the last version that completed a run |
| `~/.hole/trust.json` | project paths whose settings the user accepted, and what they accepted |
| `~/.hole/agent-versions.json` | agent versions per image ID, last fetched latest versions, skipped updates ([agents](agents.md#in-place-agent-updates)); removed by `hole uninstall` with the rest of `~/.hole` |
| `~/.hole/subpath-probe.json` | the engine version the volume-subpath probe last passed on ([configuration](configuration.md#filesexclude)) |

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

Volumes per instance, both created by Hole and referenced as `external`:

- `hole-sandbox-docker-data-<instance>` — the DinD daemon's data root, when DinD is enabled.
- `hole-sandbox-masks-<instance>` — the mask volume, when anything excluded is a directory: one
  subdirectory per excluded directory, mounted with `subpath` over it on the agent and the
  sidecar. See [configuration](configuration.md#filesexclude).

### The gateway image is also the helper image

Besides the gateway service, the gateway image hosts every one-shot host helper that needs a
shell: `hole-bridge-netfilter` (`Engine.RunHostHelper`, at start and teardown) and the mask
volume's preparation and subpath probe (`Engine.RunVolumeHelper`, at start). It is the image for
the job because it is built locally from embedded assets, pinned by content hash, garbage-collected
already, and — since `compose up gateway` runs first — guaranteed to exist before any other service
starts. The agent image is not: it is built inside `compose up agent`, so on a first start or with
`-r` it does not exist yet when the helpers run. A dedicated helper image was rejected (a pulled
one adds an unpinned registry dependency to the start path, a built one adds a second Dockerfile,
tag and GC rule for a `mkdir`), and so was renaming the image (a one-off migration that orphans
every existing `hole-gateway:*` image for no functional gain). `state.Instance.GatewayImage` is
recorded so teardown runs the helper of the version that started the sandbox.

The volume helpers run with `--rm`, `--network none` and the image's entrypoint bypassed, and are
named `<instance>-maskprep-1` / `<instance>-maskprobe-1` — the shape compose gives its own
containers — so one left behind by an interrupted start is swept by the container GC pass. The
agent version probe (`RunThrowaway`) follows the same rule as `<instance>-versionprobe-1`, and is
removed outright when it times out.
`RunHostHelper` (`--network host`, `NET_ADMIN`) is deliberately not reused for them.

## Startup sequence

1. Register the signal handlers first (`notifySignals`): from here on a signal is buffered on a
   channel instead of killing the process with nothing cleaned up. Resolve host identity, project
   name and instance ID. A caller-chosen ID (`--instance-id`) is claimed before anything is
   created (`claimInstanceID`): it is rejected when a live instance of any project uses it, or when
   an abandoned instance with the same name still has leftovers.
2. Take the run's liveness lock, clear stale `.abort`/`.stopping` markers a previous run with the
   same name may have left, create the run temp directory (after the lock, so a rejected twin
   start leaves nothing behind), write the instance state file (phase `starting`) **before any Docker
   resource exists**, spawn the teardown watchdog, and start consuming signals
   (`watchSignals`). From here on, an abort anywhere has an owner.

   The lock comes **before** the first write: a state file without a held lock and without a
   watchdog PID is `Abandoned`, so a concurrent `hole list` (whose GC runs on every call) would
   tear down the instance that is starting. The lock is also what rejects a twin start with the
   same caller-chosen ID in the same project, which `claimInstanceID` cannot see before it has
   written its state.
3. Validate and merge the settings documents (with the selected profile's chain, if any); resolve
   the agent registry and verify the startup agent is enabled.
4. Resolve `cleanupHost` hooks and snapshot the merged settings into the state file, so teardown
   works even if the next step aborts.
5. Build the egress policy and resolve the image identity. Without `-r`, adopt a sibling's
   bit-identical image (an existing `hole-sandbox/agent-*` image carrying the target tag is
   re-tagged into this project's repository, so it counts as existing from here on —
   [configuration](configuration.md#image-identity-and-scope)), print the banner, then run the
   agent version check and any in-place agent update it or `--update-agents` asked for
   ([agents](agents.md#in-place-agent-updates)) — before any network or host hook, because a
   growth-guard `Y` turns the run into a `-r` run. An update failure only warns; a signal during it
   ends the start. Then run `setupHost` hooks.
6. Run the version-change migration and garbage collection.
7. Allocate and create the two networks, then the DinD volume — in that order, so a half-started
   instance is always recognisable by its network.
8. Materialize the build contexts and gateway configuration; generate one compose file. Directory
   masks are generated as subpaths of the mask volume unless the compose plugin is older than
   2.26; the mask volume is created right after, and only when there is a directory to mask.
9. `compose up -d` per service: gateway → docker (if enabled) → agent, each through a
   [build stream](#hook-logs) that writes `build.log`, renders the build progress and splits out
   the setup hooks' logs. Between the gateway and the
   next service, the mask volume's subpaths are prepared and one is probe-mounted over the gateway
   image; if either fails, the volume is removed, a warning names the engine, and the compose file
   is regenerated with tmpfs masks (the gateway's definition is identical in both files, so compose
   leaves it running). Health gating comes from
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

Between the steps, **interruption checkpoints** check whether a signal arrived and, if so, end the
start with the normal deferred teardown: after `watchSignals`, after the trust gate, after the
agent update, after `setupHost`, after the networks, and after each `compose up` (gateway, docker,
agent). Without them a signal that reaches only the `hole` process during a slow step — where
stopping the not-yet-existing agent container has no effect — would let the start carry on to a
fully running sandbox. A signal to the whole process group (a terminal Ctrl-C, or the documented
contract for programs that drive `hole start`) also aborts the running `compose`/build child, so
the next checkpoint is reached at once; a process-only signal waits for that step to return.

Right after the mask verification of the agent (and sidecar) the phase becomes `running`
(`recordPhase`), before image GC, which can take seconds. The write takes the teardown lock
**without waiting** and is skipped when the lock is busy or the instance is no longer registered:
an agent that exits immediately is already being torn down by the watchdog, and writing the full
struct then would re-create the state file of a destroyed sandbox, while waiting for the lock
would make `attach` find the container gone and lose the agent's exit code.

The exit code the CLI reports comes from the agent *container*, not from the attach client,
whenever the container has already stopped. An agent whose command finishes before the attach
lands makes the runtime refuse with "cannot attach to a stopped container" and exit 1, which would
otherwise turn a successful short-lived agent into a failed run.

## Hook logs

Every hook script gets one log file under `~/.hole/logs/<run log name without .log>/`
(`hooks.RunLogDir`), named `<kind>-<NNN>-<name>.log` — `setup-host`, `build-setup`, `prestart`,
`cleanup-host`. The format lives in `internal/hooks/hooklog.go`: a header (kind, script, start
time, and Hole's hook variables only — never `os.Environ()`), the output, then `duration:` and a
fixed trailer as the last line (`exit code: N`, `killed by signal: SIGTERM`, `exit code: unknown
(<reason>)`, `not run: <error>`). The directory is `0700` and the files `0600`: hook output can
carry tokens. It is never the project's `.hole/logs` — that is inside the sandbox's read-write
mount (the reasoning in `dump.go`). The watchdog finds the directory from
`state.Instance.LogFile`. `LogFileGC` removes `run-<date>-<time>-*` directories with the run logs
and leaves every other directory alone, since `~/.hole/logs/<project>/` holds the `-n` dumps.

How each hook's output reaches its log:

- **`setupHost`, `cleanupHost`** — `hooks.runScript` tees stdout and stderr (one writer, so their
  order survives) to the console and the log, and reads the exit code or the killing signal from
  the `WaitStatus`. The streams become pipes as a result, and `os/exec` would wait for every
  holder of a pipe — including a background process the script left running — so `WaitDelay`
  bounds that wait, and `exec.ErrWaitDelay` counts as the script's own successful exit.
- **`setup`** — the logs are needed most when the build fails, and a failed build leaves no image,
  so nothing can be read out of the image afterwards. The Dockerfile loop prints
  `::hole-hook-begin setup <name>::` / `::hole-hook-end setup <name> exit=<rc>::` around every
  script and exits on the first failing one. `engine.ComposeUp` takes an `io.Writer` (with
  `BUILDKIT_PROGRESS=plain` in the environment) and `sandbox.buildStream` parses the stream:
  BuildKit's `#N [service x/y]` headers map vertex ids to services (compose builds `depends_on`
  services in the same call, and their vertices interleave), `#N <time> text` lines of the vertex
  that printed a begin marker feed the hook's log with the prefix stripped. Buildah's `STEP x/y:`
  and the legacy builder's `Step x/y :` lines carry no vertex id, so there every unmarked line is
  candidate hook output. A script whose end marker never appeared is reported only once the
  build's outcome is known: BuildKit clips a step's log at 2 MiB (`[output clipped …]`), losing
  the marker of a script that may have succeeded.
- **`prestart`** — the entrypoint prints the same markers and exits with the failing script's
  code. Teardown reads the agent container's log *before* `compose down`
  (`engine.ContainerLogLines` streams it and stops at the entrypoint's `Starting agent CLI...`
  line, so the agent's TUI redraws are never read and nothing the agent prints can reach the
  logs), strips ANSI sequences and collapses `\r` redraws (the container has a TTY), and splits it.
  The scanner already drops overwritten redraws (`scanTerminalLines`), so a progress bar running
  for minutes without a newline cannot outgrow its buffer. These logs carry no `duration:` — they
  are written after the fact.
  A read-write host mount for the logs was rejected: the agent runs right after and could rewrite
  or symlink them.

`hooks.Splitter` is shared by both marker-based kinds. It accepts only the names Hole itself put
in the build context or prestart directory, so an echoed marker can neither invent a file name nor
escape the directory; within those names the first begin and the last end marker count. Markers
are forgeable by the scripts, which does not matter: the loop's exit status decides the build or
the start, never a marker. A per-build nonce was rejected because it would invalidate the hook
layer's cache on every build.

The build stream also drives the console: `logging.BuildProgress` shows a single status line
rewritten in place on a terminal (the console handler clears it before every record and redraws it
after, under the same mutex), one line per step elsewhere, and a permanent line per finished setup
hook. With `-d` or `HOLE_BUILD_PROGRESS=plain` it forwards the raw stream instead. On failure the
stream reports the failing hook with its exit code, output tail and log path — or the tail of the
whole output when the failure is outside a hook — and `start.go` then skips
`reportServiceStartFailure` for that service, whose container does not exist. `ImageBuild`
(`--update-agents`) still writes to the console directly.

## Reliability: registry, watchdog, GC

### Instance registry

`~/.hole/instances/<instance>.json` records everything teardown, `hole list` and GC need:
identity, flags, the merged settings snapshot, both process IDs, networks and subnets, the DinD
volume, the run directory and log file, the start time, and the lifecycle `phase` (`starting`,
then `running`; absent in files of older versions). Writes are atomic (temp file + rename) so a
reader never sees half a file.

Next to it live small sidecar files: `.alive` (the liveness lock), `.lock` (the teardown lock),
`.abort` (the CLI asks the watchdog to tear down now) and `.stopping`. **Stopping is a marker, not
a phase value**: the CLI's signal goroutine must record it, but the main goroutine owns the
`state.Instance` struct and rewrites it whole on every `store.Write`, so writing `stopping` into
the JSON from the goroutine would be a data race and the next write would revert it. The marker is
created under the teardown lock and only while the state file exists, and `store.Remove` deletes
it, so it can never outlive the instance. `hole list --json` reports `stopping` when the marker
exists, whatever the phase says.

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
started earlier holds a snapshot with no networks in it. Its first act under the lock is dropping
the `.stopping` marker, before any slow step, so a caller polling `hole list --json` sees
`stopping` within a poll interval whichever path tears down.

Phases: prestart hook logs → `-n` dump → `compose down --remove-orphans` (fileless, `-p` only, so teardown never
depends on generated files) → registry mirror detach → explicit removal of both networks with an
attached-container force fallback → DinD volume → mask volume → registry mirror stop when this was the last
instance → `cleanupHost` hooks → run directory → state file → a final verification pass that
names any leftover and prints the command to remove it.

`isLastInstance` ([configuration](configuration.md#hooks)) is evaluated once, between the volume
and the hooks, and serves both: Hole stops the shared mirror (kept, not removed — `Ensure`
restarts it with its cache) and the hooks get the same value in `HOLE_IS_LAST_INSTANCE`.

The force fallback exempts the registry mirror: it is shared by every sandbox, and the fallback
is reached exactly when the recorded detach did not happen (the CLI killed between `Attach` and
the `store.Write` that records it, or a failed `Detach`). It is disconnected instead, which frees
the network without destroying another sandbox's image cache.

**Nothing in teardown is gated on how far startup got.** That was the root cause behind the bash
version's "cleanup seems random" symptom: `compose down` only ran when the final `up -d agent`
had succeeded, so an earlier failure leaked containers, and the network removal then failed
invisibly because containers were still attached.

Signals (INT/TERM/HUP) reach the CLI for the whole run, including the image build: they mark the
instance stopping, stop the agent container, which ends an active attach, let the interrupted step
return (the next [interruption checkpoint](#startup-sequence) ends the start), and set the exit
code to the conventional 130/143/129.

### Garbage collection

GC runs on every `start` and `list` (except `hole list --no-gc`, for callers that poll; it still
filters abandoned instances out of its output). It is the backstop for the one case the watchdog cannot
cover — CLI and watchdog killed at once — and it is deliberately conservative, because anything
it removes might belong to a concurrent start:

| Pass | Rule |
|---|---|
| Abandoned instances | CLI lock free **and** watchdog PID dead → full teardown, including running containers |
| Networks | runtime prune filtered by `hole.managed` **and** `until=10m`, so a start that created its networks but no containers yet is safe |
| Containers | stopped sandbox containers whose instance has no state file, no running sibling and **no networks left** (compose has a window where networks exist but containers do not) |
| Volumes | per-instance DinD and mask volumes whose instance has neither network nor container — volume prune has no age filter, so sibling liveness is the age proxy |
| Run directories | `~/.hole/tmp/run.*` older than a day that no registered instance owns |
| Run logs | run log files and `run-<date>-<time>-*` hook log directories older than 7 days |
| Update references | `hole-sandbox/agent-update:<instanceID>` and `:<instanceID>-base` whose instance ID is not in the registry — an update interrupted by a kill; skipped when the registry cannot be read |

Image GC is separate and runs only *after* the agent service is up — never before, or a failed
build could have destroyed the last working image. See
[configuration](configuration.md#image-identity-and-scope).

## `hole list --json` is a public contract

The JSON output is consumed by other programs (the JetBrains plugin first), which detect support
by running it once and rely on its fields from then on. It is built from its own type,
`sandbox.listEntry`, never by encoding `state.Instance`: the registry file is internal and can be
refactored freely, the list schema cannot. Fields are only ever added, never renamed or removed.
`status` is derived at listing time (`instanceStatus`: marker, then phase, legacy → `running`),
`settings` is the slice the table joins with `+` (a path can contain `+`, so it is never split
from a string), and `networks`/`settingsFiles` are `[]`, never `null`. Logging goes to stderr, so
stdout carries only the document, encoded completely before anything is written.

## Destroy

- `hole destroy <path>` — containers, networks, that project's own image repository, and DinD
  and mask volumes for one project. The shared agent image and the gateway image are preserved; they may
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
- Secrets are hidden by masking the path: files get a `/dev/null` over-mount, directories get an
  empty subdirectory of the per-sandbox mask volume (a tmpfs on an engine that cannot mount volume
  subpaths). Never a bind of a host directory — Docker Desktop's Docker VMM drops a bind nested
  inside another host bind about a second after start — and never an anonymous volume, which
  `compose down` leaks without `-v`.
- Every mask is **verified inside the running container** once, as soon as it is up and before
  the terminal is attached (`verifyMasks`, `internal/sandbox/maskcheck.go`); a missing one aborts
  the start. `docker
  inspect` keeps reporting a mount the container's kernel no longer has, so the host's view cannot
  be trusted for this.
- `files.include` and `libraries` are opt-in; libraries are read-only by default, and an include
  reaches the Docker-in-Docker sidecar only when marked `docker`.

**The agent runs as a non-root user** mirroring the host user, so files it creates in the project
have the right ownership. It does have passwordless `sudo` inside the container: the container is
the boundary, not the user. `NET_ADMIN` on the agent is likewise safe — the only route out is
through the gateway, so rewriting routes inside the agent can break its own connectivity but
never widen it.

**Project settings are untrusted input.** `<project>/.hole/settings.json` is repository content
(and a `$HOLE_PROJECT_SETTINGS_FILE` replacing it is treated as the same input),
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
escapes. The sidecar sees the project directory read-write, the exclusion over-mounts, the
libraries and the `files.include` entries explicitly marked `docker` — the libraries because a
nested container has to be able to bind-mount them, and a `:ro` one stays read-only on the same
user-namespace property the exclusions rely on. That set is always a subset of the agent's mounts,
so the sidecar gains nothing the agent — which drives it through `DOCKER_HOST` — does not already
have. What remains is a
kernel- or runtime-level escape from the privileged sidecar, which is why DinD is off by default
and documented as a larger surface than the rest of the sandbox. See
[configuration](configuration.md#docker-in-docker).
