# Go rewrite — code review

Review of the `go-rewrite` branch: ~7.3k lines of new Go (`cmd/`, `internal/`, `test/`) plus the
embedded assets under `assets/`. `go build`, `go vet` and `go test` all pass. The three HIGH
findings were verified against the source; finding 6 still needs one `nft -f` run to confirm or
kill.

## Functional coverage

Complete against the developer documentation. The settings schema's `$defs/settings` properties
match `config.Settings` field-for-field (`files`, `network.{allow,hostGatewayDomains,subnetPool}`,
`dependencies`, `container.*`, `hooks.*`, `libraries`, `environment`, `agents`,
`git.worktreeLinks`). Profiles and `extends`, subnet allocation, watchdog/GC/teardown, image
identity and scope, DinD plus the pull-through mirror, self-update, uninstall and the 1.x migration
are all implemented as described.

## HIGH

### 1. `agents.<name>.args` loses repeated flags when no profile is selected

`internal/sandbox/start.go:418`

The no-profile branch calls plain `config.Merge`, whose array semantics concatenate **and
deduplicate**. `applyAgentArgs` exists precisely to prevent that — its comment names the
`["--tool","a","--tool","b"]` failure — but only `MergeWithProfile` calls it.

Global `agents.claude.args = ["--allowedTools","Bash"]` plus project `["--allowedTools","Read"]`
yields `["--allowedTools","Bash","Read"]` on the no-profile path, versus the correct
`["--allowedTools","Bash","--allowedTools","Read"]` on the profile path. The agent is launched with
a malformed command line.

Fix: route the no-profile branch through `config.MergeWithProfile(globalDoc, projectDoc, nil)`,
which also produces the correct `globalOnly` document.

### 2. Two `hostGatewayDomains` entries for the same domain break every sandbox start

`internal/network/allow.go:261`

`BuildPolicy` stores `HostGateway` verbatim, and the CoreDNS template (`gateway.go:32`) renders one
`<domain>:53 { … }` server block per entry. `["app.test:8080","app.test:9090"]` therefore emits two
identical `app.test:53` blocks; CoreDNS refuses that config (*zone already defined*), the gateway
never reports healthy, and because both other services use `depends_on: condition:
service_healthy`, **every** `hole start` fails with nothing but compose's "container is unhealthy".

The trigger is realistic: global settings list `app.test:8080` and project settings
`app.test:9090` — array merge dedups by exact string, so both survive.

The nftables side already unions ports across entries (`Generate` builds `hostGatewayPorts`), which
shows merge-by-domain is the intended semantics. `BuildPolicy` should group `HostGateway` by domain
the same way it groups allow entries.

### 3. `ProjectName` hashes the sanitized path, so distinct projects collide

`internal/hostenv/hostenv.go:193`

`sanitizeName` strips every character outside `[a-z0-9-]` and lowercases, and the sha1 is taken over
that result:

```
ProjectName("/home/me/my_project") == ProjectName("/home/me/myproject") == "myproject-53efa994"
```

`Foo` and `foo` collide likewise. Consequence: `hole destroy ~/myproject` computes the same
`hole-sandbox-myproject-53efa994-` prefix and force-removes the **running** containers and networks
of a live `~/my_project` sandbox (`destroy.go:23-29`); the two projects also share one image
repository. Underscores in directory names are common, and the doc comment directly above claims
"two projects with the same basename therefore never collide."

Fix: `sha1.Sum([]byte(absPath))`.

## MEDIUM

### 4. Hardcoded Node patch version breaks codex/gemini on rebuild

`assets/agents/codex/command.json:2`, `assets/agents/gemini/command.json:2`

`command.json` pins `$HOME/.nvm/versions/node/v22.22.2/bin/node`, while `install-user.sh` runs
`nvm install 22`, which installs whatever the newest 22.x is *at build time*. That layer sits after
`ARG CACHEBUST`, so `-r` or any new image tag re-runs it; once Node publishes another 22.x patch the
installed directory no longer matches and the agent fails at launch with ENOENT.

Fix: pin the patch in `install-user.sh`, or make the command version-agnostic (resolve via
`nvm which` or a stable symlink).

### 5. Self-update can write to the wrong directory

`internal/update/update.go:37`

`os.Readlink` returns the link *target*, which may be relative
(`../Cellar/hole/2.0.0/bin/hole`). `replaceBinary` then applies `filepath.Dir` to that relative
string, creating the temp file and renaming relative to the process CWD — the update lands somewhere
arbitrary, the installed binary is untouched, and the CLI reports success. Darwin-only in practice
(`os.Executable` on Linux is already `/proc/self/exe`-resolved).

Fix: `filepath.EvalSymlinks(executable)`.

### 6. Overlapping IP/CIDR allow entries may generate an nftables set nft rejects

`internal/network/gateway.go:100` — needs one `nft -f` run to confirm.

The static set is emitted with `flags interval` but no `auto-merge`, and `elements` is the plain
sorted list. `network.allow: ["10.0.0.0/24:443","10.0.0.5:443"]` (same port group, so one set)
produces `elements = { 10.0.0.0/24, 10.0.0.5 }`. nft rejects conflicting intervals in an interval
set unless `auto-merge` is set, which would make `nft -f` fail, the `set -euo pipefail` entrypoint
exit 1, and the sandbox never start. `dedup` only removes *identical* entries, so
overlapping-but-unequal ones reach the template.

Fix: add `auto-merge` alongside `flags interval`.

### 7. `mergeLibraries` precedence breaks when a directory is spelled differently

`internal/sandbox/mounts.go:166`

The map is keyed by the *raw* host path but resolution happens later in `addLibraries`, so a
configured `"libraries": {"~/other-worktree": {"readwrite": true}}` and the git-derived
`/home/me/other-worktree` become two separate keys. `addLibraries` iterates `SortedKeys`, `/`
(0x2f) sorts before `~` (0x7e), and `mountBuilder.add` is first-wins by target — so the derived
read-only mount wins and the agent's writes fail, contradicting the function's own documented
derived → configured → flag precedence.

Fix: key the map by `host.ResolveHostPath(...)`.

### 8. The DinD sidecar receives all mounts, not just exclusions

`internal/sandbox/composegen.go:200`

`dindVolumes = append(dindVolumes, mounts.mounts...)` appends everything the builder collected:
exclusion over-mounts, `files.include` targets and `libraries`. Confirmed in the golden output
(`internal/sandbox/testdata/full.yml`), where the `docker` service carries
`<LIBRARY>:/libs/shared:ro`.

Both the code comment ("Exclusions are mirrored so `docker build` … cannot see the secrets") and
`README.md:314` describe exclusions only. So either the documentation is wrong or the mount set is:
host paths a user opted into for the *agent* are also handed to a **privileged** container. This
should be an explicit decision, not an incidental one.

## LOW

### 9. Network-removal fallback can destroy the shared `hole-registry` mirror

`internal/sandbox/teardown.go:111`

When `NetworkRemove` fails, every attached container is force-removed. `dindregistry.Detach` runs
only if `instance.RegistryMirror` is set in the re-read state file, and it is best-effort with a
Debug-level failure. Two ways to reach the bad path: the CLI is killed between `Attach` succeeding
and the `store.Write` that records `RegistryMirror` (`start.go:243-251`), or a `Detach` that simply
fails. Either way, teardown of one sandbox removes shared infrastructure. Capped at low because the
cache volume survives and `Ensure` recreates the container.

Fix: skip `dindregistry.ContainerName` in the attached-container removal loop.

### 10. `hole list` uses a PID check the architecture doc explicitly rules out

`internal/sandbox/list.go:41`

`state.PIDAlive(instance.CLIPID)` is exactly the test `architecture.md` says is wrong ("a process
that has exited but not been reaped still answers signal 0, so a PID check would call a dead CLI
alive"), and `PIDAlive` additionally returns true on `EPERM`, so a reused PID owned by another user
reads as alive. `store.CLIGone` is the intended predicate and is already used by GC and the
watchdog. Narrow in practice because the preceding `GC` usually removes the state file first, but it
is a stated invariant violated in code with a one-line fix.

## Checked and clean

- `compose.Marshal`'s `$` → `$$` escaping, external volume/network key rendering, and the `"$$@"` in
  the DinD entrypoint wrapper — all verified against `internal/sandbox/testdata/full.yml`.
- Sandbox container-name resolution (`docker`, `hole-registry`) is **not** broken by the NXDOMAIN
  catch-all: Docker keeps `127.0.0.11` as the in-container resolver and treats compose `dns:` as its
  *upstreams*, so internal names still resolve.
