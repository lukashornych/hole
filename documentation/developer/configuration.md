# Configuration (`settings.json`)

Two optional documents share one schema (`assets/schema/settings.schema.json`, embedded in the
binary):

- `~/.hole/settings.json` — global defaults
- `<project>/.hole/settings.json` — per-project settings

User-facing reference and examples live in the [README](../../README.md#configuration); this page
documents the mechanics.

## Loading and validation

`config.LoadAndValidate` reads a file, checks it for **removed 2.0 keys**, then validates it
against the schema. The order matters: a removed key would otherwise surface as a bare
"additional properties not allowed", which tells the user nothing about what replaced it.

The schema is strict — `unevaluatedProperties: false` at the root and inside every profile — so
any new setting **must** be added to it or every user's startup breaks. See
[recipes](recipes.md#add-a-settings-option).

Library settings files go through the same pipeline, but only their `files.exclude` is honored.

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
the sandbox), `container.docker` (privileged sidecar), `hooks.setup`, `dependencies` (code during
the image build, which uses the host's unfiltered network). Nothing else is gated: `files.exclude`
only removes access, and `network.allow`, `environment`, `agents.*.args`, `container.baseImage` and
`hooks.prestart` act inside the container, which is the boundary. Gating `network.allow` was
considered and rejected — it is the most common thing a project file contains, and a prompt that
fires on nearly every project trains the user to accept it, which is what would then also accept a
`setupHost` script.

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
  `$VAR` would leave it unchanged.
- **What re-prompts.** `~/.hole/trust.json` records a sha256 over the grant set per project path,
  so an ungated settings edit keeps the decision valid while a project that starts asking for more
  asks again. Keying on the grant set rather than on the path is what closes the obvious loop: the
  project is mounted read-write, so an agent can add `hooks.setupHost` to the file it lives in, and
  the next start must not inherit the trust the file had before. Such an edit cannot affect the
  run it happens in either: teardown replays `cleanupHost` from the snapshot taken at the trusted
  start, never from the file. An unreadable record counts as untrusted — the failure direction has
  to be a prompt. `~/.hole` is not part of any sandbox mount
  unless the user mounts it explicitly, which is itself a gated `files.include`.

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

Each entry becomes an over-mount: files get `/dev/null:<path>:ro`, directories get an empty
directory created under the run temp dir. Never anonymous volumes — `compose down` leaks those
without `-v`, and the run dir is wiped in teardown anyway.

Entries containing `*`, `?` or `[` are globs, expanded by a custom matcher because stdlib
`filepath.Glob` has no `**`. It walks segment by segment, so a `**` pattern prunes instead of
scanning everything. Overlapping matches are deduplicated by container target. Exclusions are
mirrored onto the DinD sidecar.

### `files.include`, `libraries`, `git.worktreeLinks`, `--library`

All three sources fold into one map keyed by the **resolved** host path, with precedence
**derived → configured → flag**: an explicit entry always beats one Hole worked out on its own.
Keying by the raw value would break that: `~/other-worktree` and the git-derived
`/home/me/other-worktree` are the same directory but two keys, and both then reach the mount
builder, where first-wins-by-target quietly keeps whichever sorts first.

`--library PATH[:MOUNT][:rw]` defaults to `/libs/<basename>`, read-only unless `:rw`.

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

### Docker-in-Docker

A `docker:dind-rootless` sidecar on the internal sandbox network, in a **privileged** container
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
the same over-mount `umount` both fail. The outer container stays privileged because rootlesskit
needs it (unprivileged, or `SYS_ADMIN`+`NET_ADMIN` without `--privileged`, the daemon starts but
cannot run a single container — it fails at the session-keyring and `/proc` mount steps), so this
is defense-in-depth: a kernel or runtime escape from the privileged sidecar can still reach the
host, and that residual risk is documented rather than closed. Data root, pid path and the
privilege drop are the three things the rootless image changes from the root one; getting any of
them wrong fails silently (a healthy-looking daemon that cannot run containers, or a sidecar with
no route to the gateway), which is why they are called out here.

The sidecar receives the project mount and **only the exclusion over-mounts**
(`mountBuilder.exclusions`), never `files.include` targets or `libraries`. Mirroring an over-mount
can only ever remove access; mirroring an exposed path would hand it to the sidecar for no reason —
the daemon does not need it, since builds stream their context from the client (`docker build` and
`buildx` work against paths the daemon cannot see) and only a run-time bind mount needs a
daemon-side path. Keeping the sidecar's view minimal is the conservative default regardless of the
daemon's privilege level.

1.x passed the whole mount set here while its comment and README both said exclusions only — the
wider set came from reusing one array, not from a decision. `TestDinDSidecarReceivesExclusionsOnly`
pins the split.

Each instance gets a fresh named volume for the daemon's data root
(`/home/rootless/.local/share/docker`, not `/var/lib/docker` — rootless stores under the
unprivileged user's home), since concurrent sandboxes must not share one. A fresh volume inherits
the image's `rootless:rootless` ownership of that path on first mount, so the daemon can write to
it without a chown step. Caching comes from the pull-through mirror instead:
`internal/dindregistry` runs a long-lived `hole-registry` container (upstream `registry:2` in
proxy mode) on its own bridge network, attaches it to the sandbox network at start and detaches it
at teardown so the network stays removable. Sandbox-internal traffic is not filtered — the gateway
polices egress to the internet only — so the daemon reaches the mirror even under default-deny.
Every failure here is non-fatal: DinD without a cache simply pulls from the internet.

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
   user agents.

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
| `network.*` | no — runtime config mounts |
| `environment`, `agents.*.args` | no — compose environment / command |
| `container.memoryLimit`, `memorySwapLimit` | no — compose limits |
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

**Image GC** runs after the agent service is up — never before, or a failed build could have
destroyed the last working image. It removes the other tags of the chosen repository, drops the
project's own repository entirely when the shared image was chosen, and prunes dangling images
restricted to Hole's `com.hole.image` label so a user's unrelated leftovers are never touched.

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
