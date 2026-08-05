# Go rewrite code review — verdicts and fix plan

Verification of every finding in [go-rewrite-code-review.md](go-rewrite-code-review.md) against the
source on `go-rewrite`, with the two open questions settled empirically (CoreDNS 1.12.1 and
`nft` 1.1.6 in containers).

**Result: 10 of 10 findings are real. No false positives.** Two severities need re-grading and one
finding needs its history corrected — it is inherited 1.x behavior, not a rewrite regression.

**All ten are fixed.** Each fix carries a regression test; 2 and 6 additionally got golden cases.

| # | Finding | Verdict | Severity | Status |
|---|---|---|---|---|
| 1 | `agents.*.args` dedup on the no-profile path | confirmed, reproduced | HIGH | **fixed** |
| 2 | duplicate `hostGatewayDomains` kill every start | confirmed, reproduced | HIGH | **fixed** |
| 3 | `ProjectName` collisions | confirmed, reproduced | HIGH | **fixed** |
| 4 | hardcoded Node patch version | confirmed (**inherited from 1.x**) | MEDIUM | **fixed** |
| 5 | self-update follows a relative symlink | confirmed | MEDIUM | **fixed** |
| 6 | overlapping IP/CIDR set rejected by nft | **confirmed empirically** | HIGH, not MEDIUM | **fixed** |
| 7 | `mergeLibraries` keyed by raw path | confirmed, reproduced | MEDIUM | **fixed** |
| 8 | DinD sidecar gets all mounts | confirmed — never an explicit decision | MEDIUM | **fixed** |
| 9 | teardown can remove the shared mirror | confirmed | LOW | **fixed** |
| 10 | `hole list` uses a PID check | confirmed | LOW | **fixed** |

## Evidence

### 1 — reproduced

```
no-profile path (config.Merge):        [--allowedTools Bash Read]
profile path (MergeWithProfile, nil):  [--allowedTools Bash --allowedTools Read]
```

The suggested fix is right: `MergeWithProfile(global, project, nil)` degenerates to the plain
two-way merge *and* runs `applyAgentArgs`. It also fixes the `globalOnly` document the same way,
which matters because that document feeds the image-scope comparison.

### 2 — reproduced, and the consequence is exactly as claimed

`BuildPolicy` stores `HostGateway` verbatim, so `["app.test:8080","app.test:9090"]` emits two
`app.test:53` blocks. Loading that Corefile into `coredns/coredns:1.12.1`:

```
cannot serve dns://app.test.:53 - it is already defined
container state: exited exit=1
```

The gateway container dies, never reports healthy, and both dependent services block on
`service_healthy` — so every `hole start` fails for that configuration, not just one feature.

Fix: group `HostGateway` by domain in `BuildPolicy`, unioning ports, exactly as it already groups
allow entries — and as `Generate` already does when it builds `hostGatewayPorts` for nftables. The
port-less "all ports" case must win over any port list for the same domain.

### 3 — reproduced

```
/home/me/my_project        -> myproject-53efa994
/home/me/myproject         -> myproject-53efa994
/home/me/MyProject         -> myproject-53efa994
/home/me/my.project        -> myproject-53efa994
```

`destroy.go:23-29` builds `hole-sandbox-<projectName>-` and force-removes containers and networks
by that prefix, so `hole destroy ~/myproject` tears down a **running** `~/my_project` sandbox. They
also share one image repository and one DinD volume namespace.

Fix: hash the unsanitized absolute path (`sha1.Sum([]byte(absPath))`), keeping `sanitizeName` for
the human-readable basename only.

Two consequences to accept deliberately:

- **Every existing project name changes.** Cached images and any registered instances from before
  the fix become orphans. Images are reclaimed by the image GC and the version-change migration;
  instance files are only a concern if one is live during the upgrade. Harmless, but it should be
  noted in `MIGRATION.md` rather than discovered.
- **Case-different spellings of the same directory** on a case-insensitive filesystem (macOS
  default) become two project names instead of one — `/Users/me/x` vs `/Users/me/X`. That is a
  duplicate sandbox, which is strictly better than today's cross-project destroy, and
  `ResolveProjectDir` already normalises symlinks so the usual paths agree.

### 4 — confirmed, but not a rewrite regression

The pin came from 1.x commit `4123ccd`, whose subject — *"fix: gemini and codex agents don't use
fixed node version"* — says the opposite of what the diff does: it **replaced** `["codex", …]` with
the absolute `$HOME/.nvm/versions/node/v22.22.2/bin/{node,codex}`. The rewrite carried it over
faithfully.

That history constrains the fix. The absolute path exists because the agent CLI is `exec`'d without
nvm's shell initialisation, so reverting to `"codex"` reintroduces the problem `4123ccd` was solving.

**Decided and applied: pin both sides to the same patch version.** `install-user.sh` now runs
`nvm install 22.22.2` / `nvm use 22.22.2` for codex and gemini, matching the path in `command.json`,
with a comment in each file naming the coupling. `claude` is unaffected — its `command.json` is
`["claude", …]`.

The duplication is the obvious objection, so it is guarded rather than trusted:
`TestPinnedNodeVersionMatchesTheInstallScript` parses the pinned version out of every agent's
`command.json` and fails unless some install script installs exactly that version. Verified it
catches drift by editing one side. Bumping Node is now a deliberate two-file edit, and forgetting
the second file fails `make test` instead of the sandbox.

A rejected alternative, for the record: symlink `$HOME/.hole/node` at the resolved version and
reference that. It removes the duplication but hides which version is in use and adds a build step;
the explicit pin is easier to reason about for a security tool. (The symlink must not live under
`~/.nvm/versions/node/` — nvm enumerates that directory as its version list.)

### 5 — confirmed

`os.Readlink` returns the link target verbatim, which is relative for a Homebrew-style
`../Cellar/hole/2.0.0/bin/hole`. `filepath.EvalSymlinks(executable)` is the correct call. Narrow in
practice — `install.sh` writes a real file — but the failure mode is the bad kind: the update lands
somewhere arbitrary and the CLI reports success.

### 6 — settled: the review's suspicion was correct, and the severity is understated

Generated for `network.allow: ["10.0.0.0/24:443","10.0.0.5:443"]`:

```
set g0_static { type ipv4_addr; flags interval; elements = { 10.0.0.0/24, 10.0.0.5 } }
```

Loaded with real `nft`:

```
Error: conflicting intervals specified
     elements = { 10.0.0.0/24, 10.0.0.5 }
                  ~~~~~~~~~~~  ^^^^^^^^
```

With `auto-merge` added it loads and collapses to `elements = { 10.0.0.0/24 }` — the union, which is
the intended meaning.

**Re-grade to HIGH.** `nft -f` failing means the `set -euo pipefail` entrypoint exits 1, so the
blast radius is identical to finding 2: the gateway never starts and every `hole start` fails while
that allow list is in force. Only the trigger is narrower.

### 7 — reproduced

```
key /home/dev/other-worktree -> readwrite=false   (git-derived)
key ~/other-worktree         -> readwrite=true    (configured)
```

Both survive as separate keys. Running the merged map through `addLibraries` produces exactly one
mount, and it is the wrong one:

```
mount: <home>/other-worktree:<home>/other-worktree:ro
```

`addLibraries` iterates sorted keys, `/` (0x2f) precedes `~` (0x7e), both spellings resolve to the
same container target, and `mountBuilder.add` is first-wins by target — so the derived read-only
mount wins and the user's explicit `readwrite: true` is silently discarded. Fix: key the merge map
by `host.ResolveHostPath(...)`.

### 8 — confirmed; this one is a decision, not a defect

The golden output shows the privileged `docker` service carrying `<LIBRARY>:/libs/shared:ro`
alongside the exclusion over-mounts, while both the code comment and `README.md:314` describe
exclusions only.

The sharpest argument for deciding it explicitly is that **a `:ro` mount is not a boundary against a
privileged container** — tested, not assumed:

```
unprivileged, :ro          -> sh: can't create /data/f: Read-only file system
privileged, :ro + remount  -> remount ok / WROTE to a read-only mount
content afterwards         -> hacked
```

The sidecar is privileged by necessity and the agent drives it through `DOCKER_HOST`, so anything
mounted into it read-only is reachable read-write. Mirroring `libraries` there therefore weakens the
read-only default that is the whole reason `libraries` are `:ro` — while the exclusion over-mounts,
which only ever *hide* things, cost nothing to mirror.

#### What the history says

The question "was this ever an explicit decision?" is answerable, and the answer is no.

DinD arrived in `d41716e` (*"feat: support Docker containers and compose inside agent sandbox"*).
At that commit `agent_volumes` **already** accumulated exclusions, `files.include` targets *and*
libraries — and the sidecar was handed the whole array under this comment:

```sh
# Mirror file exclusion volumes on DinD container
for v in "${agent_volumes[@]}"; do
```

The README added by the *same* commit says:

> **File exclusions:** Exclusion volumes from the agent are mirrored on the DinD container's
> `/workspace` mount, so `docker compose` files cannot access excluded secrets.

So the intent was recorded twice — in the comment and in the user-facing docs — and both say
exclusions. The wider set is an artifact of reusing one array, and the documentation has been wrong
since the feature's first commit. The Go rewrite inherited the behavior, the comment and the
mismatch.

Note what the same commit *did* state deliberately: "The project directory is mounted at
`/workspace` in both the agent and DinD containers, so bind mounts in user `docker-compose.yml`
files resolve correctly." The project mount is an explicit decision; it is kept separately in the Go
code and is not in question here. Libraries were never mentioned.

#### Decided and applied: exclusions only

`mountBuilder` now tracks which mounts came from `addExclusions` and the sidecar receives only
those, plus the project mount. `TestDinDSidecarReceivesExclusionsOnly` asserts the split in both
directions — no library or include reaches the sidecar, every exclusion does — with the include
backed by a real file, so the negative assertion cannot pass for the wrong reason. Documented in
`README.md`, `MIGRATION.md` (it narrows observable behavior) and `configuration.md`.

**The build-context counter-argument does not apply**, which was worth settling before accepting the
narrowing. The docker client tars the context and streams it over the API; the daemon never reads
the path. Proven in an environment whose daemon cannot see the client's filesystem — the same path
bind-mounts as an empty directory, yet `COPY` from it succeeds:

```
docker run -v <path>:/x  ->  path invisible to daemon: mounted as empty DIR
docker build .           ->  COPY proof.txt succeeded: content-from-client-filesystem
```

So `docker build` and `buildx` are unaffected, including the pending buildx feature request. What
does need a daemon-side path is a **run-time** bind mount (`docker run -v /libs/shared:...`, or a
compose `volumes:` entry), which is exactly the case 1.x documented for the project directory — and
the project mount is retained.

#### Rationale (kept for the record)

Mirror the **exclusions only**, matching the stated intent, and note it in `MIGRATION.md` since it
narrows observable behavior. Reasoning: the mirroring exists to *hide* things, that purpose is fully
served by exclusions, and libraries default to `:ro` precisely to prevent writes — a guarantee the
privileged sidecar cannot uphold.

The counter-argument, for the record: a `docker build` or `docker compose` run inside the sandbox
that references a library path as build context would stop working. That is a real if narrow
regression risk, and it is the reason to decide rather than assume. Nothing in the history suggests
anyone relied on it.

Implementation note: exclusions are currently indistinguishable from the rest inside
`mountBuilder.mounts`, so the builder needs to track which mounts came from `addExclusions`
(a second slice, or a flag per mount) rather than the sidecar filtering by shape.

### 9 — confirmed

`removeNetworksOf` force-removes every attached container when `NetworkRemove` fails, and
`dindregistry.Detach` runs only when `instance.RegistryMirror` was recorded. Skip
`dindregistry.ContainerName` in that loop.

Related, same root cause, already observed in this repo's own test suite: after the mirror is
removed, an immediate `Ensure` can find a container mid-removal (`marked for removal and cannot be
connected`) and degrade to no cache. Best-effort by design, so no production change was made — but
if 9 is fixed, consider whether `Ensure` should wait out a `removing` state.

### 10 — confirmed; the fix is two lines, not one

`state.PIDAlive` is the check `architecture.md` explicitly rules out, and it returns true on
`EPERM`, so a reused PID owned by another user reads as alive.

`store.CLIGone` is the intended predicate but covers **only the CLI** — the watchdog holds no lock —
so the replacement is:

```go
if store.CLIGone(instance.InstanceName) && !state.PIDAlive(instance.WatchdogPID) {
    continue
}
```

## What was applied

In the recommended order — 6 and 2 together (one golden refresh), then 3, 1, 7, 5, and finally
9 and 10.

| # | Change | Regression test |
|---|---|---|
| 6 | `auto-merge` on the static interval set (`gateway.go`) | golden case `overlapping-statics` (`10.0.0.0/24:443` + `10.0.0.5:443`) |
| 2 | `mergeHostGateway` in `BuildPolicy`: group by domain, union ports, port-less absorbs a port list, sorted output | `TestBuildPolicyMergesHostGatewayDomains` (asserts the Corefile defines each zone once) + golden case `duplicate-host-gateway` |
| 3 | `ProjectName` hashes `absPath`, not `sanitizeName(absPath)` | `TestProjectNameDistinguishesPathsThatSanitizeAlike` over the four colliding paths |
| 1 | `loadSettingsDocument` routes the no-profile path through `MergeWithProfile(…, nil)`; the `profiles_test` helper mirrors it | `TestAgentCommandKeepsRepeatedFlagsWithoutAProfile` asserts the launched command vector; verified it fails against the old code |
| 7 | `mergeLibraries` keys by `ResolveHostPath`, and `addLibraries` no longer resolves a second time (expansion is not idempotent) | `TestMergeLibrariesResolvesSpellingsOfTheSameDirectory` asserts the resulting mount, not the map |
| 5 | `resolveInstallPath` uses `filepath.EvalSymlinks`, keeping the original path on error | `TestResolveInstallPathFollowsARelativeSymlink`, including the unresolvable case |
| 9 | `removeNetworksOf` filters the mirror out of the force-removal list **and** detaches it — otherwise a network whose only attachment is the mirror would leak instead | `TestWithoutRegistryMirrorKeepsSandboxContainers` |
| 10 | `runningInstances` uses `store.Abandoned` | `TestRunningInstancesUsesTheLivenessLockNotThePID` (live PID, no liveness lock) |

Documentation: `MIGRATION.md` (3), `README.md` (2), `architecture.md` (3, 9),
`networking.md` (2, 6), `configuration.md` (1, 7).

### 2 and 6 re-verified against the real daemons

A golden file proves what was rendered, not that anything accepts it — and for these two the
whole finding *is* a daemon refusing the config. Both new golden artifacts were loaded, with the
entrypoint's placeholders substituted:

```
nft 1.1.6, overlapping-statics/nftables.rules   -> loaded; set collapses to elements = { 10.0.0.0/24 }
same file with auto-merge stripped              -> Error: conflicting intervals specified
mixed/nftables.rules (regenerated)              -> loaded
CoreDNS 1.12.1, duplicate-host-gateway/Corefile -> serves app.test.:53 once, starts clean
same file with the zone block duplicated        -> cannot serve dns://app.test.:53 - it is already defined
```

The comment lines now sitting between `flags interval` and `auto-merge` are accepted inside a set
block — worth confirming, since the original empirical run had the two statements adjacent.

One thing deliberately left alone while fixing 2: once any `hostGatewayDomains` entry is
port-less, the nftables rule accepts every port to the gateway IP, including for domains that did
name ports. That is inherent — all of them resolve to one address, so per-domain port filtering is
impossible at the IP layer.


Findings 2 and 6 are both self-identifying now: a gateway that dies at startup has its container log
and last healthcheck output printed by `reportServiceStartFailure`, so the CoreDNS `already defined`
and nft `conflicting intervals` messages reach the console instead of only compose's
"container is unhealthy".

Each fix needs a regression test; 2 and 6 should additionally be golden-file cases, since both are
generated-artifact bugs that a golden diff would have exposed.

## What this review does not cover

A clean sweep here is not a sufficient gate. Three bugs found by *running* the sandbox during this
session are absent from the review, and all three are of a kind static reading does not reach:

- the gateway healthcheck could never pass (`nslookup` asks AAAA; the health zone answered only A);
- the watchdog treated a *created* agent container as started, so `docker wait` returned instantly
  and teardown ran mid-startup;
- teardown deregistered the instance before logging its final lines, so the CLI stopped relaying and
  dropped them — including the warnings that name resources it could not remove.

Timing, process-interaction and container-runtime semantics need `make e2e` and real runs. The
review and the suite are complements, not substitutes.
