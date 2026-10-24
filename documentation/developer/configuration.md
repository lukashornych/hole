# Configuration (`settings.json`)

Two optional documents share one schema (`assets/schema/settings.schema.json`, embedded in the
binary):

- `~/.hole/settings.json` — global defaults
- `<project>/.hole/settings.json` — per-project settings

The global document's path comes from `hostenv.Host.GlobalSettingsFile()`: `HOLE_SETTINGS_FILE`
(read once in `DetectHost`) replaces it when set, for wrappers that generate their own global
settings ([issue #46](https://github.com/lukashornych/hole/issues/46),
[analysis](../analysis/config-dir-override.md)). Nothing else follows it: `Host.Home`, `HoleDir()`
and therefore agents, trust, the instance registry, `tmp/`, logs, GC and the image identity are
unchanged — moving those would split the runtime registry that GC and `isLastInstance` rely on.
`Start` validates the override first (`ValidateSettingsFile`: absolute, existing regular file); a
missing override is fatal rather than a silent fall-back to defaults. The path the run actually read
is recorded in `Instance.SettingsFiles`, which `hole list` labels from (`global(<path>)`), not from
the listing process's own environment.

The main project's document comes from `Host.ProjectSettingsFile(projectDir)`:
`HOLE_PROJECT_SETTINGS_FILE` replaces `<project>/.hole/settings.json` when set. It occupies the
project slot of the same two-document pipeline, so `ResolveChain`, `MergeWithProfile`, the
`globalOnly` image baseline and the trust gate are unchanged; the in-repo file is not read at all
(its `files.exclude` included — hiding repository secrets is then the wrapper's job), and
`loadSettingsDocument` logs that it was ignored. `ValidateSettingsFile` checks it like
`HOLE_SETTINGS_FILE`. Libraries, derived worktrees and pool checkouts never follow it
(`addCheckoutExclusions` keeps reading `<checkout>/.hole/settings.json`). The override is recorded
in `Instance.ProjectSettingsFile`, from which `hole list` labels it `project(<path>)` — positional
inference from `SettingsFiles` is ambiguous when no global file exists.

User-facing reference and examples live in the [README](../../README.md#configuration); this page
documents the mechanics.

## Loading and validation

`config.LoadAndValidate` reads a file, checks it for **removed 2.0 keys**, then validates it
against the schema. The order matters: a removed key would otherwise surface as a bare
"additional properties not allowed", which tells the user nothing about what replaced it.

The schema is strict — `unevaluatedProperties: false` at the root and inside every profile — so
any new setting **must** be added to it or every user's startup breaks. See
[recipes](recipes.md#add-a-settings-option).

Library settings files go through the same pipeline, but only their `files.exclude` is honored, and
it is combined with the global `files.exclude` rather than replacing it.

### Scaffolding settings files

`hole settings create|open|get` (`internal/cli/settings.go`, `internal/config/scaffold.go`) manage
the two documents directly ([issue #53](https://github.com/lukashornych/hole/issues/53)).

- **The template is generated in code** (`config.SettingsTemplate`) from `schemaResourceURL`, so the
  `$schema` URL users write cannot drift from the one the compiled schema is registered under. It is
  deliberately not an embedded asset: `assets.BuildInputsHash()` digests every embedded file into
  the agent and gateway image tags, so a template file would rebuild both images for every user.
- **`--append` is an ordered shallow merge** on the raw top-level fields (`AppendTemplate`), not a
  round trip through `Document`, whose map would sort the keys and reformat the file. A conflicting
  top-level key takes the template's value whole; missing keys are inserted first. Non-objects,
  trailing data and duplicate top-level keys are errors rather than being silently dropped. The
  result is not schema-validated: the user may be fixing a broken file, and `start` validates anyway.
- **Writes are plain `os.WriteFile`**: no concurrent reader needs the temp-file-and-rename dance, and
  writing in place keeps the file mode and follows a symlinked (dotfiles-managed) file.
- **The overrides are followed**: the target comes from `Host.GlobalSettingsFile()` /
  `Host.ProjectSettingsFile(dir)`, exactly as `start` reads it, and the project path argument is
  validated with `ResolveProjectDir` even when an override ignores it. `ValidateSettingsFile` is not
  reused because it requires the override to exist, which `create` must not; `settings` only
  requires a set override to be absolute.
- **`open` reports what `start` would**: it validates with `LoadAndValidate` under the same labels as
  `loadSettingsDocument`, and re-checks that the file still exists after the editor exits, since
  `LoadAndValidate` treats a missing file as valid. The editor runs as `sh -c '<editor> "$1"'`, so
  `EDITOR="code -w"` works and the path is never spliced into the script.

### Removed keys and migration errors

`network.domainWhitelist`, `network.allowedPorts` and the scalar `hooks.setup` object were removed
in 2.0. `config.CheckRemovedKeys` detects them and returns a `MigrationError` carrying a
paste-ready replacement.

The 1.x → 2.0 translation lives **only** in that hint generator, never in runtime behavior:
silently reinterpreting an old allow list would leave the user believing a policy is in force that
Hole never applied. Because tinyproxy matched whitelist entries as unanchored regexes, each domain
is suggested as both an exact and a wildcard entry so reachability is preserved; `allowedPorts: []`
(the old `ConnectPort 0`) is called out as the block-everything case rather than turned into
reachable entries.

## Project trust

The global file is the user's own document. The project file is repository content, so the
settings in it whose effect **leaves the sandbox** are gated behind a per-project confirmation
(`internal/trust`):

`hooks.setupHost`, `hooks.cleanupHost` (host code), `files.include`, `libraries` (host paths into
the sandbox), `container.docker` (privileged sidecar), `git.worktreePool` (creates a host directory
next to the project and mounts it read-write), `network.hostGatewayDomains` (services on
the developer's machine), `hooks.setup`, `dependencies` (code during the image build, which uses
the host's unfiltered network), `network.allow` (egress widening). Nothing else is gated:
`files.exclude` only removes access, and `environment`, `agents.*.args`, `container.baseImage`,
`hooks.prestart` and `network.subnetPool` act inside the container, which is the boundary.
`network.bridgeNetfilterFix` does touch the host firewall, but the rule it controls is scoped to
the sandbox's own bridge and opens nothing a sandbox does not already have — see
[networking](networking.md#hosts-that-filter-bridged-packets-br_netfilter).

The asymmetry inside `git` is deliberate. `worktreePool` is gated because it *creates* a directory
outside the project; `worktreeLinks: "rw"` mounts checkouts of the repository Hole was already
pointed at, and gating it now would re-prompt every project already using it. The capability
contributes nothing when the setting is false or absent, so no existing recorded digest changes.

The network keys are gated for the same reason as the rest: the test is whether an effect **leaves
the sandbox**, and egress does. The gateway still polices `network.allow` at L3/L4, but it polices
it *to the widened set* — a repository that adds `attacker.example.com` gets a full-duplex channel
for the project's contents, which is the harm the default-deny policy exists to prevent, and the
gateway enforcing a policy the repository wrote is not a boundary. Prompt fatigue is the cost: the
gate fires once per project and again whenever the grant set changes, and `--trust-project` covers
automation. Gating only allow entries that name private/loopback addresses was considered and
rejected — an allow entry can be a *name*, which resolves at runtime, so no syntactic check can
tell LAN reach from public reach
([analysis](../analysis/host-gateway-mandatory-ports-plan.md)).

Three details carry the design:

- **Where the gate runs.** In `Start`, immediately after the settings load and **before** the
  merged snapshot reaches the instance registry. Teardown replays `cleanupHost` from that snapshot
  (`teardown.go:runCleanupHostHooks`) without checking how far startup got, so a gate placed after
  the snapshot write would decline a project's script and then run it.
- **What is trusted.** `trust.Grants` reads the project document *before* the merge, so the prompt
  shows what the repository asks for rather than what merging produced, and it scans the base
  settings **and every profile** — the file is the unit of trust, so which profile a run selects
  neither hides a grant nor invalidates a recorded decision. Values are kept **raw**: an expanded
  path embeds the host's home, which would make the digest machine-specific, and a redirected
  `$VAR` would leave it unchanged. A value may carry a suffix for the part of the grant a bare path
  does not show — ` (read-write)` on a library, ` (docker)` on an include mirrored onto the
  privileged sidecar. **The unsuffixed rendering is frozen**: it is what every recorded digest was
  taken over, so changing how a plain entry renders would re-prompt every trusted project. Adding
  the flag to an include does change the digest, which is the intent — it widens what the
  privileged container can bind-mount.
- **What re-prompts.** `~/.hole/trust.json` records a sha256 over the grant set per project path,
  so an ungated settings edit keeps the decision valid while a project that starts asking for more
  asks again. Keying on the grant set rather than on the path is what closes the obvious loop: the
  project is mounted read-write, so an agent can add `hooks.setupHost` to the file it lives in, and
  the next start must not inherit the trust the file had before. Such an edit cannot affect the
  run it happens in either: teardown replays `cleanupHost` from the snapshot taken at the trusted
  start, never from the file. An unreadable record counts as untrusted — the failure direction has
  to be a prompt. `~/.hole` is not part of any sandbox mount
  unless the user mounts it explicitly, which is itself a gated `files.include`.
- **`HOLE_SETTINGS_FILE` is trusted like the default global file**, so it must not be writable from
  inside the sandbox. `checkSettingsFileNotExposed` (`internal/sandbox/mounts.go`) refuses a start
  when the symlink-resolved file lies under the project, a `files.include` source, a library
  (configured, `--library`, git-derived) or the worktree pool. It runs right after the trust gate,
  before any host hook. The default `~/.hole/settings.json` is not checked — mounting `~/.hole` is
  already a gated choice. Scripts the global file references are out of scope, as they are for the
  default file.
- **`HOLE_PROJECT_SETTINGS_FILE` is gated like the in-repo file** — `trust.Gate` sees its document
  and names its path, and trust stays keyed by `ProjectDir` and the grant-set digest. It therefore
  needs no exposure check: an agent that can write it can only produce a grant set the next start
  prompts for, the same as editing `<project>/.hole/settings.json`. Only `--trust-project` on every
  run removes that prompt, which the README warns about.

Without a terminal the start fails, naming the file, the keys and `--trust-project`; that flag
accepts the current grant set non-interactively and records it. Failing to *write* the record is a
warning, not an error: the user has already given the answer for this run.

This supersedes §3 item 5 of
[analysis/dind-hardening-fix-plan.md](../analysis/dind-hardening-fix-plan.md), which proposed
honoring `container.docker` only from the global file. Trust-on-first-use makes it a prompted grant
instead of a refused one, so a project that legitimately needs a daemon still works — do not
implement both.

## Merge semantics

Merging happens on the untyped document, which is then decoded into `config.Settings`:

- **objects** merge recursively, the higher-precedence document winning;
- **arrays** concatenate lower-precedence first, then deduplicate preserving first occurrence;
- **scalars** and type mismatches are overwritten.

The type-mismatch rule is load-bearing for the string-or-object settings. A higher-precedence file
restating `files.include["~/.m2/settings.xml"]` as `{"path": ..., "docker": true}` overwrites the
global string form and thereby opts the entry into the Docker sidecar; the reverse direction drops
the flag. Both are the expected "higher precedence wins" outcome, and neither needs special-casing.
`libraries` behaves the same way.

One exception: `agents.<name>.args` is recomputed as a plain concatenation of every contributing
source. Generic dedup would corrupt an argument vector — `["--tool", "a", "--tool", "b"]` would
lose its second flag and bind `b` to the first. The exception lives in `MergeWithProfile`, so
**every** merge goes through it: with no profile selected the chain is simply empty, which
degenerates to the plain two-way merge and keeps the argument handling.

## Profiles

Profiles are named overlays under a top-level `profiles` key, selected with
`hole start <agent>:<profile>`. The agent positional splits on the **first** colon, so an agent
name can never contain one. A profile with any command other than `start` is fatal, as is a name
violating `^[a-z0-9][a-z0-9-]*$` — checked by the CLI before any settings file is read, so a typo
fails clearly even in a project with no settings at all.

A requested profile must exist in at least one file. Absent is **fatal** rather than ignored:
running with the base permissions instead of the ones the profile grants would be a silently wrong
sandbox. The error lists what each file defines.

Application order: global base → global overlays in chain order → project base → project overlays
in chain order. This preserves the invariant that anything in the project file overrides anything
global, while keeping the leaf profile the highest-precedence overlay within each file. `profiles`
and `extends` are metadata, stripped before merging, so the merged document never contains them
and the no-profile case degenerates to the plain two-way merge.

`extends` (string or array) expands depth-first into an ordered list — parents before children,
each name applied once, so diamonds are harmless under additive merge. The `extends` view is the
array merge of both files' declarations, so a project profile can extend a globally-defined one and
vice versa. An unknown parent or a cycle is fatal, and the cycle error names the path.

**Additive only, deliberately.** Replace and remove semantics were rejected because effective
permissions would then require mentally evaluating subtraction across four sources. Two documented
consequences: keep the base minimal, and put a mount whose *source* varies between profiles inside
each profile — `files.include` is keyed by host path, so a base mount and a profile mount can
otherwise collide on one container path. That collision is a fatal startup error naming both host
sources.

### Schema structure

To avoid duplicating every property, all root settings except `$schema` live in
`$defs/settings`, which has **no** `additionalProperties: false`. Strictness moves to the call
sites via `unevaluatedProperties`, which — unlike `additionalProperties` — sees properties
evaluated through a `$ref`. The root is `$ref: #/$defs/settings` plus `$schema` and `profiles`;
each profile is `$ref: #/$defs/settings` plus `extends`, with `unevaluatedProperties: false`. So a
profile accepts exactly the root keys, but not `profiles` (no nesting) and not `$schema`.

Cycle and existence checks for `extends` are runtime checks; JSON Schema cannot express them.

## Path resolution

Every path-valued setting goes through one pipeline: trailing slashes stripped, `$VAR`/`${VAR}`
expanded (an undefined variable warns and stays literal), `~/` resolved against the host home or —
for container-side paths — the sandbox home, and relative paths resolved against the project
directory.

Unlike the bash implementation, an expanded value is **not** re-scanned for further references,
which removes the class of infinite loop that version had to guard against with placeholders.

Do not hand-roll path handling elsewhere; use `hostenv.Host.ResolveHostPath` /
`ResolveContainerPath`.

## Error-handling policy

Carried over verbatim from 1.x:

- **User config problems that don't compromise the sandbox** (missing excluded path, glob with no
  matches, missing hook script, undefined variable, missing include or library): `logging.Warn`
  and skip.
- **Problems that make the sandbox wrong or unsafe** (schema violation, removed key, unknown
  agent or profile, malformed allow entry, colliding include targets, failed network allocation):
  return an error; the CLI prints it and exits non-zero.
- **Teardown never aborts**: best-effort, warnings only, and every leftover named.

## Settings reference (implementation view)

### `files.exclude`

Each entry becomes a mask on the container path: files get an over-mount of `/dev/null:<path>:ro`,
directories get their own subdirectory of the sandbox's **mask volume**
`hole-sandbox-masks-<instance>`, mounted over the path with the long-form volume syntax
(`type: volume`, `volume.subpath: "<n>"`) on the agent and the DinD sidecar alike. Neither has a
source on the host, and that is the point — a bind of an empty host directory (what 2.x did until
the Docker VMM finding) is nested inside the project's own host bind, and Docker Desktop's libkrun
VMM tears such a mount down about a second after the container starts, silently unhiding the
directory. A volume's source lives inside the VM and survives; that was measured for subpath
mounts on Docker VMM, Apple Virtualization and Colima (see
[the analysis](../analysis/docker-desktop-vmm-exclusion-loss.md)).

The volume is disk-backed, so a build writing gigabytes into an excluded `target` or
`node_modules` lands on Docker's disk (on macOS, the VM's bounded disk image) and disappears with
the sandbox — the semantics the host placeholder had. It is **one volume per sandbox**, not one per
directory: the excluded directories across the project, every library and every pool worktree can
number in the dozens, and each would otherwise cost an API call, a `docker volume ls` line and a GC
iteration. Its lifecycle is the DinD volume's: created by `Start` after the networks and the
instance registration (so GC cannot sweep a concurrent start), recorded as
`state.Instance.MaskVolume`, removed by teardown, `hole destroy` and the GC volume pass. It is
created only when at least one excluded path is a directory.

`subpath` requires the subdirectory to exist, and a fresh volume is `root:root 755` while the agent
runs unprivileged. So after `compose up gateway`, `prepareMaskVolume` runs two helpers over the
gateway image ([architecture](architecture.md#the-gateway-image-is-also-the-helper-image)): one
mounts the whole volume, `mkdir -p`s every subpath and `chown`s it to the agent's uid/gid; the
second mounts one prepared subpath and runs `true` — the honest, runtime-agnostic check that the
engine can mount subpaths at all (podman's compatibility API does not follow Docker's API
numbering, so an API-version gate would misroute it).

The probe's answer only changes with the engine, so a success is cached in
`~/.hole/subpath-probe.json`, keyed by the runtime binary plus its CLI and engine versions
(`engine.ClientServerVersion`, one `version` call instead of a container run). A start whose key
matches skips the probe; any version change — a Docker update — misses the key, probes again and
overwrites the record, so nothing ever has to invalidate it. Only successes are recorded: a failed
probe may be transient, and caching it would pin the sandbox to tmpfs. An engine whose versions
cannot be read is always probed. The key cannot see a change that keeps every version, such as
switching Docker Desktop's VM backend; a stale "supported" then surfaces as a failed `compose up`,
never as an exposed path, and `verifyMasks` checks every mask regardless. Deleting the file forces
a fresh probe.

**The tmpfs fallback.** Subpath mounts need Docker Engine 26 / API 1.45 and Compose 2.26. Hole
masks directories with a tmpfs instead — the service's `tmpfs` key, with `exec` because the
runtime's default is `noexec` and an excluded `node_modules` or `target` holds binaries a build
then runs — when either check fails:

- `compose version --short` parses as a version below 2.26.0 (an unparsable version is trusted and
  left to the probe). podman-compose reports its own 1.x numbering and so always falls back: the
  probe runs `podman run`, which cannot vouch for how podman-compose writes a subpath mount, and
  that has not been verified. The check is made while the compose file is generated, and only when
  there is a directory to mask (`composeInput.subpathUnsupported`), so a sandbox without one never
  runs `compose version`. No volume is created.
- The preparation or the probe fails. The compose file has already been written for the volume and
  the gateway is already up, so the file is regenerated with tmpfs masks; the gateway's definition
  is the same in both, so compose leaves it running. The volume is then removed.

Either way a `logging.Warn` names the engine and its version, gives the reason — for a failed
probe the engine's own error, e.g. a docker CLI older than 26 that does not know `volume-subpath`
— and says that excluded directories
are now RAM-backed and count against `container.memoryLimit`. It is warn-and-continue, not an
error: an old engine is a user-environment limitation, the tmpfs is just as safe, and its only cost
is memory — which the warning makes visible instead of letting a build be OOM-killed with no
message tying it to the exclusion.

Every mask is recorded in `mountBuilder.masks`, the single record every consumer derives from:
`exclusionMounts()` for the sidecar mirror of file masks, `volumeMasks()` / `tmpfsMasks()` for the
directory masks of both services, `maskSubpaths()` for the preparation run, and `maskTargets()` for
the check against the container's own `/proc/self/mountinfo` once it is up; see
[the mask check](#the-mask-check). Subpaths are numbered in recording order, so both generations of
one start agree on them.

Entries containing `*`, `?` or `[` are globs, expanded by a custom matcher because stdlib
`filepath.Glob` has no `**`. It walks segment by segment, so a `**` pattern prunes instead of
scanning everything. Overlapping matches are deduplicated by container target. Exclusions are
mirrored onto the DinD sidecar.

Three deliberate properties of the matching, all recorded because the failure direction favors
exposure (see [analysis/security-audit.md](../analysis/security-audit.md), findings 9 and 10):

- **A pattern that matches nothing warns and the start continues.** Exclusions are meant to be
  written once, globally, for files only some projects have (`.env`, `**/*.pem`) — erroring would
  make a shared default forbid every project that does not happen to contain the path. The cost is
  that a typo hides nothing and only warns.
- **Globs do not descend symlinked directories.** `config.ExpandGlob` classifies with `os.ReadDir` +
  `entry.IsDir()`, i.e. Lstat semantics, which is what bash `globstar` does — the semantics the
  matcher documents itself as implementing. So `secrets/**` matches nothing when `secrets` is a
  symlink, and warns. A symlinked path named *directly* is excluded correctly: `addExclusions`
  checks existence with `Lstat` (a dangling link still warns) but decides file-vs-directory with
  `Stat`, because `/dev/null` over a directory is a mount the runtime rejects.
- **Three sources, three scopes.** The **global** `files.exclude` (the profile-resolved
  `documents.globalOnly`, threaded through `composeInput.globalExclude`) applies to the project *and*
  to every other checkout Hole mounts. The **merged** settings — which already contain the global
  patterns — drive the project mount. A **checkout's own** `.hole/settings.json` adds to the global
  set for its mount alone; the project's patterns never reach it.
- **`files.exclude` has no reach into `files.include` targets.** Patterns are resolved against the
  project directory and, separately, against each library's own mount. An included path — `~/.claude`,
  `~/.m2/repository` — is mounted whole or not at all.

### The mask check

`generateCompose` returns every masked container path alongside the compose file, and `Start`
asserts each one is a mount point inside the agent container — and inside the DinD sidecar, which
carries the same masks — before attaching the terminal. A path that is not masked aborts the
start, so the deferred teardown destroys the sandbox; it is the "makes the sandbox wrong or
unsafe" branch of the error-handling rule, never a warning.

The check runs once, as soon as `compose up` returns. The agent process is already running by
then — compose starts it — so this is not a gate in front of the agent; it is the earliest point
at which the container's mount table can be read at all, and the directory masks it checks —
volume subpaths or tmpfs — have no host source for a runtime to drop later. A subpath mount is a
mount point in `mountinfo` exactly like a tmpfs, so the check is the same for both.

Four things make the check what it is:

- **It runs inside the container.** `docker inspect` reports the mounts the runtime was *given*,
  and keeps reporting them after the kernel has dropped one, so a host-side check cannot see the
  failure it exists to catch. `Engine.Exec` reads `/proc/self/mountinfo` instead.
- **It compares whole paths.** `/project/secrets` being mounted says nothing about
  `/project/secrets2`, and the kernel's octal escapes (`\040` for a space) are decoded first, or a
  project path with a space in it would make every exclusion under it look dropped.
- **It follows symlinks the way the runtime did.** A mount destination is resolved inside the
  rootfs before mounting, so an excluded `linked-secrets -> secrets` is listed in the mount table
  under `secrets`. Each target is first resolved with `readlink -f` inside the container, looked
  up under that path, and reported under the name the settings used.
- **An exited container is skipped, not refused.** A non-interactive run whose agent has already
  finished has nothing left to protect, and failing it would fail a run that did its work. The
  state is read only after an exec fails, never before: a container can exit between the two
  calls, and a pre-check would turn that into a refusal of a run that did its work.

The one runtime known to need this is Docker Desktop's Docker VMM (libkrun); on macOS the error
says so and points at the Apple Virtualization framework. That hint is a message and nothing more —
the check runs on every host and every runtime, because the next quirk of this shape will not
announce itself either.

### `files.include`, `libraries`, `git.worktreeLinks`, `git.worktreePool`, `--library`

All three sources fold into one map keyed by the **resolved** host path, with precedence
**derived → configured → flag**: an explicit entry always beats one Hole worked out on its own.
Keying by the raw value would break that: `~/other-worktree` and the git-derived
`/home/me/other-worktree` are the same directory but two keys, and both then reach the mount
builder, where first-wins-by-target quietly keeps whichever sorts first.

`--library PATH[:MOUNT][:rw]` defaults the mount point to the host path itself — like the project
and the worktree-derived links, and unlike the configured `libraries`, whose container path is
always explicit. A basename-derived `/libs/<name>` would break every reference that records an
absolute path (symlinks, `go.mod` replaces, tool caches), which is exactly what a library is for.
Read-only unless `:rw`.

`files.include` values are string-or-object (`config.Include`, decoded like `config.Library`): the
string is the container path, and the object form adds `docker: true`, which mirrors the entry onto
the DinD sidecar at the same container path with the same options. `mountBuilder.dockerIncludes`
collects the flagged mounts, the third mirrored category next to `libraries` and `exclusions`.
Unlike `libraries`, `path` carries no `pattern` constraint — include values may be relative and
resolve against the project directory, so tightening it would break existing files.

The two categories differ in one runtime respect: libraries (including the worktree-derived ones)
are mirrored onto the DinD sidecar unconditionally, `files.include` targets only where flagged —
see [Docker-in-Docker](#docker-in-docker).

**The sidecar's mount set is a subset of the agent's, by construction.** A "sidecar-only" mount
would be a fiction: the agent holds `DOCKER_HOST`, so anything the daemon can bind-mount it can
read with `docker run -v <path>:/x alpine cat /x`, and listing such a path under a sidecar-only
grant would make the trust prompt claim a separation that does not exist. The only real lever is
whether a nested container can bind-mount the path *directly*, which is exactly what the flag
controls. One entry with one container path also removes a whole class of silent failure: two
independent maps would let the two sides disagree on the path, and the nested bind would then
resolve to an empty directory instead of erroring.

The subset property is also why no new collision domain is needed. Everything mirrored is an agent
mount, so the builder's single `seen` set — one mount per container target — already guarantees
uniqueness on the sidecar. A flagged entry the builder *rejected* (missing host path, target
already claimed) is recorded nowhere, the same rule `addLibraries` follows.

Worktree derivation (`internal/worktree`): in a linked worktree the main repository is mounted — a
linked worktree's `.git` is only a pointer, so git would not work without it — and in a main
repository every linked worktree outside the project directory is mounted, each at its own
absolute path so tooling that records paths keeps working. git is optional: a missing binary or a
non-repository yields no links, never a failed start.

Every path comparison there happens on symlink-resolved paths, because git resolves symlinks in
everything it prints: against an unresolved project directory nothing git reports would ever match,
and a plain repository would look like a linked worktree of itself. `hostenv.ResolveProjectDir`
already resolves the path for CLI callers, so `worktree.Derive` resolving again is belt-and-braces —
but it is what keeps the function correct on its own terms, and on macOS every path under `/tmp` or
`/var/folders` is symlinked.

`git.worktreePool` adds a place for the agent to *create* worktrees in, on top of the mechanism
above, which only exposes the ones that already existed. `Derive` returns a `Derivation`: the links,
the pool directory, and the checkouts that already exist inside it. The decision tree, in order:

```
worktreeLinks unset or off      → nothing (one master switch for the whole mechanism, off unless set)

project is not a worktree root  → nothing (a subdirectory of a checkout, main or linked)

project is a linked worktree    → link the main repository (ro/rw); no pool

project is the main repository
    ├─ core.bare == true     → link every linked worktree outside the project; no pool
    ├─ worktreePool == false → link every linked worktree outside the project
    └─ worktreePool == true  → mount `<project>-worktrees` read-write
                             + link every linked worktree outside the project *and* the pool
```

Five things carry that design:

- **The project directory has to be a worktree root.** `git rev-parse --show-toplevel` is the probe:
  it names the root of the working tree the directory belongs to — for main and linked worktrees
  alike, as a physical path, which is what the rest of the function compares against — and it fails
  only where there is no working tree, so a failure is accepted exactly when `core.bare` says the
  project is a bare repository. Below a root the derivation is both useless and harmful: `.git` sits
  above the project mount, so git cannot work inside the sandbox whatever gets linked, and linking
  the enclosing checkout would hand the agent the whole repository the narrower project directory
  was chosen to keep out. The older `isInside(mainRepo, project)` guard tested the containment the
  other way around and let that case through.
- **Pool mode never activates in a linked worktree.** That is what keeps the pool a *sibling* of the
  project, so the pool mount and the project mount can never nest, and makes the path trivially
  `project + "-worktrees"`. It costs nothing git allows anyway: from a linked worktree whose main
  repository is mounted read-only, `git worktree add` cannot write `.git/worktrees/` either.
- **A checkout inside the pool gets no individual link.** With both unfiltered, a pool holding two
  checkouts would produce `ro` library mounts nested inside the `rw` pool mount, and `mountBuilder.add`
  keys on the container target — the targets differ, so which mount the agent ends up seeing would come
  down to runtime mount ordering. Excluding them partitions the space instead: project mount, pool
  mount, individual link, by location.
- **The pool directory is named in `internal/worktree` and created in `start.go`** (`ensureWorktreePool`,
  before `generateCompose`). `Derive` stays side-effect free — it is unit-tested with no runtime and
  writes nothing — while the creation has to happen host-side and as the invoking user: a bind source
  the daemon creates is root-owned, and against a remote daemon a missing one silently resolves to an
  empty directory. It is the rare mount failure that is fatal rather than a warning, because the agent
  would be told it has a pool and would write into a directory that dies with the sandbox.
- **`HOLE_WORKTREES_DIR` is the whole interface.** `composegen` exports it only when the pool is really
  mounted, so its absence is a reliable signal, and nothing generates instructions for the agent: the
  convention belongs in the project's own `CLAUDE.md`/`AGENTS.md`/`GEMINI.md`, which is already mounted.
  Writing into the project would dirty the user's checkout, and appending to `$HOME/.claude/CLAUDE.md`
  would edit the user's real global memory whenever they mount their agent config — which is the
  documented pattern.

Exclusions follow one rule: **every checkout Hole exposes hides what the global settings and its own
`.hole/settings.json` ask to hide, scoped to its own mount** (`mountBuilder.addCheckoutExclusions`,
used by `addLibraries` and by `addPoolWorktreeExclusions`). The global half is what makes the
documented "write your secret patterns once, globally" advice hold for a checkout that carries no
settings file; an entry named by both sources resolves to one container target, which the builder's
`seen` map already keeps exactly once. Inside the pool it is load-bearing: the pool is a *single* mount at its
root, so without it `<pool>/feature/.env` would be visible while the same checkout mounted as its own
library is protected. The children get masks, not mounts of their own — source and target are
the same absolute path, so they land inside the pool mount exactly like the project's own exclusions.
The child list comes from `git worktree list`, so unrelated directories a user parked in the pool are
left alone. The project's own `files.exclude` deliberately does not reach into them, for the same
reason it does not reach into a library: a pool child and a sibling checkout must hide the same files.

Consequences, accepted: a worktree created *mid-session* gets no exclusions at all, because the mount
set is fixed at start — the sharp edge of the mode's benefit, documented in the README — and a
checkout on a branch whose `.hole/settings.json` predates an exclusion does not hide what that
exclusion hides, exactly like a library in that state — weakened, since global patterns cover such a
checkout and only checkout-specific ones are missed.

The bare-clone layout (`repos/proj.git` plus per-branch worktrees beside it) is the one case where
the common directory is not a `.git` *inside* a working tree but the repository itself, so
`filepath.Dir` on it lands on an unrelated parent — every sibling the user parked in `repos/` would
be mounted. `Derive` therefore probes `git config --get core.bare` and, when it is true, takes the
common directory as the main repository unchanged. **The probe has to be `config --get core.bare`,
not `rev-parse --is-bare-repository`**: the latter answers for the *current* worktree and returns
false from a linked worktree of a bare repository, which is exactly the case that matters. Stripping
a trailing `.git` element lexically would be wrong twice over — a bare repository need not be named
`*.git`, and a normal repository has a `.git` directory whose parent *is* what we want.

Consequently a bare project directory takes the main-repository branch, and the bare-clone layout has
no pool: `<project>-worktrees` would be named after the `.git` suffix, and a repository with no
working tree has no working-tree convention for a pool to belong to.

### `dependencies`

Joined into the `EXTRA_PACKAGES` build arg. apt runs during the image build, which uses the host's
network, so the Ubuntu repositories do **not** need to be in `network.allow`.

### `network.*`

See [networking](networking.md).

### `hooks.*`

| Hook | Where | When | On failure |
|---|---|---|---|
| `setupHost` | host | before any Docker work | aborts startup (`cleanupHost` still runs) |
| `setup` | container, image build | when the image is built | aborts the build |
| `prestart` | container | every start, before the agent CLI | aborts startup |
| `cleanupHost` | host | teardown | warning only |

Every entry is a literal path or a glob resolved through one shared code path
(`hooks.Resolve`). Matches take the pattern's place in entry order, sorted lexicographically, so
`NNN-` prefixes give a predictable sequence. This is non-breaking by construction: in 1.x a path
containing glob characters always failed the file-exists check and was warned about.

`prestart` scripts are copied into the run dir with numbered prefixes and mounted read-only.
`setup` scripts are copied into the build context and their **content** feeds the image hash.

`cleanupHost` runs from the settings snapshot in the state file, because the watchdog cannot
re-read files that may have changed — and it runs without a TTY.

`hookEnvironment` is shared by `setupHost` and `cleanupHost`; `cleanupHookEnvironment` wraps it
with the teardown-only `HOLE_IS_LAST_INSTANCE`, so the variable cannot leak into startup, where
nothing has exited and the answer would always be `false`.

`isLastInstance` answers it from the registry, reusing `runningInstances` — the same
`Abandoned` predicate as `hole list` and GC, not a PID check. Two details it has to get right:
the exiting instance is still registered while its hooks run (deregistering is teardown's last
act, `teardown.go`), so it is excluded **by name**; and abandoned instances are ignored, since
counting one would pin a user's shared infrastructure until the next manual cleanup. A failed
registry read reports `false` — withholding the signal delays a cleanup, whereas a wrong `true`
tears down infrastructure that live sandboxes are still using.

Hole is its own first consumer of that answer: `Teardown` computes it once and uses it to stop the
shared registry mirror (`dindregistry.Stop`) before passing the same value to the hooks, so a hook
can never be told the opposite of what Hole just acted on.

### Docker-in-Docker

A `docker:dind-rootless` sidecar — referenced by digest, not by tag, so the daemon inside every
sandbox cannot change under a fixed Hole version ([build & release](build-and-release.md#pinned-third-party-artifacts))
— on the internal sandbox network, in a **privileged** container
(rootless dind still requires it — see below) whose **daemon runs unprivileged**. Its entrypoint
runs as root (`user: root`) only to point the default route at the gateway — a route change needs
`NET_ADMIN`, which the unprivileged user lacks — and to clear the stale `meta.db-lock` and
`docker.pid` a hard-killed daemon leaves in the instance volume, then `exec su rootless` drops to
the unprivileged user before starting dockerd. The agent gets `DOCKER_HOST=tcp://docker:2375` (no
TLS — internal network only), and because the rootless daemon exposes no `/var/run/docker.sock`,
the sidecar's own healthcheck reaches it over that same TCP endpoint rather than the default
socket.

**Why rootless.** A root dind daemon lets the agent start a nested `--privileged` container that
reads the host's block devices directly, and lets a privileged process strip the exclusion
over-mounts below — both verified escapes (see
[analysis/security-audit.md](../analysis/security-audit.md), findings 1 and 3). Under
`docker:dind-rootless` the daemon runs as an unprivileged user in a user namespace, so a nested
container maps to a subuid that owns none of the host's device nodes: the same raw-disk read and
the same `umount` of a mask both fail — a directory mask is inherited into the nested user
namespace `MNT_LOCKED` exactly as an over-mount is. The outer container stays privileged because rootlesskit
needs it (unprivileged, or `SYS_ADMIN`+`NET_ADMIN` without `--privileged`, the daemon starts but
cannot run a single container — it fails at the session-keyring and `/proc` mount steps), so this
is defense-in-depth: a kernel or runtime escape from the privileged sidecar can still reach the
host, and that residual risk is documented rather than closed. Data root, pid path and the
privilege drop are the three things the rootless image changes from the root one; getting any of
them wrong fails silently (a healthy-looking daemon that cannot run containers, or a sidecar with
no route to the gateway), which is why they are called out here.

The sidecar receives the project mount, then `mountBuilder.libraries`, then
`mountBuilder.dockerIncludes`, then `mountBuilder.exclusionMounts()`, then the directory masks —
`mountBuilder.volumeMasks()` at the end of the list, or `mountBuilder.tmpfsMasks()` in its own
`tmpfs` key on the fallback. Libraries have to be there: the
daemon resolves `-v` paths in its own filesystem, and an unmirrored one is not an error but a
silently empty directory in the nested container. Exclusions have to be there so that path cannot become a
way to read what the agent was meant not to see. `files.include` reaches it **only per entry**,
through `docker: true` — the default stays off the privileged container, and the flagged subset
exists because the practical case the 2.0 hardening overlooked is real: a build tool's
configuration file (`~/.m2/settings.xml`, `~/.npmrc`) cannot go in `libraries`, which rejects
anything that is not a directory. Builds themselves need none of it, since they stream their
context from the client (`docker build` and `buildx` work against paths the daemon cannot see) and
only a run-time bind mount needs a daemon-side path.

**The mirrored set is always a subset of the agent's**, and that is what keeps the trust prompt
honest — see [`files.include`](#filesinclude-libraries-gitworktreelinks-gitworktreepool---library).

**The order is load-bearing.** A library contributes exclusion over-mounts *inside* its mount point; emitting the library bind after them would let it land on top
and unhide the excluded file. Flagged includes sit between the two for the same reason exclusions
come last: nothing that hides a path may be buried under a bind added afterwards. Moby does sort
mounts by destination depth, but that is the engine's implementation detail, not something to lean
on.

Volume masks follow that rule — they are appended last, on the agent as on the sidecar. The tmpfs
fallback is the one exception, and it has no choice: it lives in a separate compose key, so its
position among the volumes cannot be expressed at all and the runtime's destination-ordered
handling is exactly what it leans on. Both are pinned by `make itest`:
`TestVolumeMaskHidesContentOfTheMountItSitsIn` and `TestTmpfsMaskHidesContentOfTheMountItSitsIn`
assert against a real runtime that a mask nested in another mount hides its content instead of
being shadowed by it.

A mirrored `:ro` library keeps its read-only-ness for the same reason a mirrored over-mount cannot
be unmounted: the daemon is rootless, so mounts it inherits into its user namespace are
`MNT_LOCKED` and the kernel refuses to clear `MS_RDONLY`. Measured against a real sandbox — a
nested `--privileged` container's `mount -o remount,rw,bind` is refused with `permission denied`
and the write still fails with `EROFS`, while the same probe against a `readwrite: true` library
succeeds, so the refusal is the mount being enforced rather than the probe being broken. The
residual is the sidecar *container* itself: an escape from it lands outside that namespace, as
real root, where the remount works again. Defense-in-depth, not a hard boundary.

1.x passed the whole mount set here while its comment and README both said exclusions only — the
`files.include` half of that came from reusing one array, not from a decision; `docker: true` makes
it a decision. `TestDinDSidecarReceivesExclusionsAndLibraries`,
`TestDinDSidecarReceivesFlaggedIncludes`, `TestDinDSidecarMountsLibraryBeforeItsExclusions`,
`TestDinDSidecarMountsIncludesBeforeExclusions`, `TestDinDSidecarKeepsALibraryReadOnly`,
`TestFlaggedIncludeIsInertWithoutTheSidecar` and
`TestDinDSidecarSkipsIncludesThatWereNotMounted` pin the split, the order and the options.

Each instance gets a fresh named volume for the daemon's data root
(`/home/rootless/.local/share/docker`, not `/var/lib/docker` — rootless stores under the
unprivileged user's home), since concurrent sandboxes must not share one. A fresh volume inherits
the image's `rootless:rootless` ownership of that path on first mount, so the daemon can write to
it without a chown step. Caching comes from the pull-through mirror instead:
`internal/dindregistry` runs a long-lived `hole-registry` container (upstream `registry:2` in
proxy mode, also digest-pinned — and because `Ensure` restarts a stopped mirror rather than
recreating it, a new pin only reaches it once the container is removed) on its own bridge network,
attaches it to the sandbox network at start and detaches it
at teardown so the network stays removable. The teardown that finds itself last
(`isLastInstance`, [hooks](#hooks)) also stops the container — stops, never removes: the cache
volume and the container are what make the next start cheap, and `Ensure` restarts it. A sandbox
starting at that exact moment can lose its mirror to the race, which degrades exactly like a
mirror that crashes later — the daemon falls back to the upstream registry. Sandbox-internal traffic is not filtered — the gateway
polices egress to the internet only — so the daemon reaches the mirror even under default-deny.
Every failure here is non-fatal: DinD without a cache simply pulls from the internet.

`Ensure` does not accept the mirror on the strength of `docker run -d` succeeding: the registry
aborts during startup when it cannot reach its upstream, and the daemon was then pointed at a dead
endpoint. `waitUntilServing` requires the container to stay up for a couple of seconds without the
runtime restarting it — container state only, never a log line, so an upstream change of wording
cannot fail a healthy mirror — and restarts are counted from the probe's start, so a mirror that
crashed once days ago and has served since is still usable. A mirror that fails the probe is warned
about (with the registry's own last log line) and removed, keeping the cache volume; the restart
policy is `on-failure:5` rather than `unless-stopped`, so a crash-looping container cannot outlive
the sandbox that asked for it. The probe catches a mirror that never came up, not one that dies
later — for that case dockerd falls back from a failing `--registry-mirror` to the upstream registry.

That same unfiltered reachability is why the attachment is gated: `start.go` attaches the mirror
only when `policy.AllowsDockerHub()` holds, and otherwise warns and leaves `RegistryMirror` empty
(so `composegen` omits `--registry-mirror` and the daemon has no Hub path at all). The gate reads
the *resolved* policy rather than `settings.Network.Allow`, which is what makes it agree with actual
reachability: it also sees each agent's `allow.txt` and `-u`, where every host is allowed anyway and
withholding the cache would remove a capability without removing any access. See
[networking](networking.md#docker-hub-is-a-capability-token) for the token itself.

## Image identity and scope

Images are tagged with the first 12 hex characters of a sha1 over a manifest of:

1. the **canonical image configuration** — base image, enabled agents, dependencies in merge
   order, and the **content** hashes of the setup scripts, all normalized so "explicitly set to
   the default" and "not set" are indistinguishable;
2. **host identity** — username and home always, UID/GID on Linux, so two host users sharing a
   daemon never collide;
3. **Hole's own build inputs** — the digest of the embedded assets, plus file hashes of enabled
   user agents. Both leave out the update-only files (`update-*.sh`, `version.json`,
   `Dockerfile.update`; `assets.UpdateOnlyFile`): they are not part of the image, and the update
   build reads the current ones, so changing one must not rebuild anything.

`CACHEBUST` is deliberately absent: it is a rebuild trigger, not configuration.

The **gateway** image is shared by every sandbox — its configuration files are runtime mounts, so
no user setting changes its content — but it is tagged by the embedded-assets digest all the same
(`hole-sandbox/gateway:<12 hex>`). Its Dockerfile and entrypoint ship inside the binary, and
compose never rebuilds a tag that already exists, so the fixed `:latest` tag the rewrite plan
specified would leave every existing installation running the gateway it first built, with no
upgrade able to replace it. Superseded gateway tags are collected by the same image GC pass.

| Setting | Affects the image? |
|---|---|
| `container.baseImage` | yes — `BASE_IMAGE` build arg |
| `dependencies` | yes — `EXTRA_PACKAGES` build arg |
| `container.enabledAgents` | yes — which install scripts enter the build context |
| `hooks.setup` | yes — script *content* |
| `files.*`, `libraries`, `git` | no — runtime mounts |
| `network.*` | no — runtime config mounts (`bridgeNetfilterFix` acts on the host firewall at start, not on any image) |
| `environment`, `agents.*.args` | no — compose environment / command |
| `container.memoryLimit`, `memorySwapLimit` | no — compose limits |
| `container.agentUpdateCheck` | no — runtime only: decides whether the start compares agent versions; an accepted update changes the image *content* but never its tag (below) |
| `container.docker`, `--with-docker` | no — sidecar only; the Docker CLI and its compose/buildx plugins are always baked in |
| `hooks.prestart` | no — runtime read-only mount |
| `hooks.setupHost`, `cleanupHost` | no — host-side |

Because any change to an image-affecting input produces a new tag, the tag is missing locally and
`compose up` builds it automatically. **`-r` is no longer needed after a settings change**; it
remains the way to refresh versions under an unchanged configuration.

**Scope.** The canonical configuration of the merged settings is compared with that of the global
file alone: identical means the project does not change image content, so it uses the shared
`hole-sandbox/agent-global` repository; different means it gets `hole-sandbox/agent-<project>`,
and the start banner names the differing keys. The comparison is between canonicalised
configurations, not "does the project file mention image keys" — a project repeating a global value
verbatim, or adding a dependency that deduplicates away, legitimately keeps the shared image. With
a profile selected the baseline keeps the profile applied, because a *global* profile is still
global. Project names always end in an 8-hex path hash, so none can collide with the literal
`agent-global`.

Normalization **reuses the code paths the build uses** (the enabled-agents resolver, the
dependency list, the shared path pipeline for scripts). Otherwise the scope decision and the actual
build context could diverge.

**Adoption.** The repository name is keyed by path (`sanitize(basename)-sha1(absPath)[:8]`) and
carries no content information, so sibling checkouts of one project land in different repositories
even when their images would be bit-identical. Right after the image is resolved — before the
banner and the agent version check, so an adopted image counts as existing — `adoptExistingImage`
(`internal/sandbox/imageadopt.go`) looks for any `hole-sandbox/agent-*` image already carrying the
target tag and re-tags it into this project's repository; compose then finds the image present and
skips the build. The tag hashes the *closed* set of build inputs — every build argument and every
piece of build context maps onto a manifest field — so a shared tag is a sufficient proof that two
images were produced from identical inputs on the same host identity. The candidate glob deliberately
includes `agent-global`, in both directions: the two repositories can only share a tag when their
canonical configurations are genuinely identical. Adoption is skipped under `-r` and is entirely
best-effort — any failure just leaves compose to build as before. The generated compose file does
not change; only whether the image is already there when compose looks.

The one input the tag does not capture is upstream drift, since `CACHEBUST` is excluded, so an
adopted image may hold older apt/npm versions than a fresh build. That is not a new staleness class:
the same tag in the *same* repository is already reused across days. `-r` remains the escape hatch.

**In-place agent updates** (`--update-agents`, or a `Y` at the start's version prompt) are the one
way an image's content changes under an unchanged tag: a thin layer built from
`assets/agents/Dockerfile.update` on top of the current image replaces it under the same reference
(see [agents](agents.md#in-place-agent-updates)). The manifest is untouched, so the tag stays and
sibling workspaces adopt the updated image like any other. The flip side is that updates are not
sticky: any settings change that yields a new tag builds from the cache with the CLI versions of the
original build. `container.agentUpdateCheck` is not trust-gated — it only decides whether the host
fetches version numbers from URLs the agent plugins (builtin or `~/.hole/agents`, never the project)
declare.

**Image GC** runs after the agent service is up — never before, or a failed build could have
destroyed the last working image. It removes the other tags of the chosen repository, drops the
project's own repository entirely when the shared image was chosen, and prunes dangling images
restricted to Hole's `com.hole.image` label so a user's unrelated leftovers are never touched.
Adoption needs no change here: removal goes through `rmi <repository>:<tag>`, which *untags* while
another reference remains, so one project superseding its tag leaves an adopted image alive for the
others, and the image only disappears once all of them have.

## Compose generation

One file per run, from typed structs in `internal/compose`. Every string value is escaped so
Compose performs **no** interpolation: generated values are already final, and a `$` in a user's
settings must not be substituted again. A literal `$` is therefore written `$$` — which is also how
the DinD entrypoint wrapper gets its `"$@"`.

Map iteration is sorted before it reaches output (`config.SortedKeys`), so compose files and image
hashes are reproducible and golden-testable.

One field of the generated file comes from the host rather than from settings: the agent service's
`tty`/`stdin_open` pair follows whether the CLI has a terminal, which is why the golden set carries
a `non-interactive` case alongside the others. It does not affect image identity — the image is the
same either way.

**Escaping shifts the expansion job onto Hole.** 1.x relied on Compose interpolating the generated
file, which is how `"PROJECT_PATH": "$PROJECT_PATH"` in `environment` ever worked. With escaping in
place, anything a user could previously reference through Compose must be expanded here or it
reaches the container literally. Two settings feed unescaped-looking user text into compose values
and therefore go through `expandContainerValue`: `environment` values and `agents.<name>.args`.
Everything else is either path-valued (already expanded by the shared pipeline) or not a place a
variable reference belongs. Arguments from the command line are deliberately left alone — the
user's shell already expanded them, and re-expanding would defeat their quoting.
