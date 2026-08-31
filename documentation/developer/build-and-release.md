# Build & release

## Local build

```sh
make build                  # ./hole, static, version "development"
VERSION=2.0.0 make build    # stamped, as a release build is
```

The version is injected with `-ldflags -X github.com/lukashornych/hole/v2/internal/version.Version`.
A checkout build carries no version of its own — it reports `development (<sha>)`, with `, dirty`
appended when the tree has uncommitted changes, skips update checks, runs no migrations and refuses
to self-update, so it can never overwrite itself with a release. A `go install` build is unstamped
too but does have an identity; see [build identity](#build-identity).

## Pinned third-party artifacts

Three artifacts Hole downloads at runtime are pinned, because a floating reference lets their
content change under a fixed Hole version — and two of them are security-relevant enough that the
change would go unnoticed:

| Artifact | Pinned in | Form |
|---|---|---|
| CoreDNS release tarball | `assets/gateway/Dockerfile` (`COREDNS_VERSION` + `COREDNS_SHA256_*`) | version + per-arch sha256, verified with `sha256sum --check --strict` before extraction |
| Docker-in-Docker sidecar (`docker:dind-rootless`) | `internal/sandbox/composegen.go` (`dindImage`) | image digest |
| Registry mirror (`registry:2`) | `internal/dindregistry/dindregistry.go` (`Image`) | image digest |

CoreDNS is the DNS half of the filtering gateway, so installing it on nothing but TLS would hold
the policy engine to a lower standard than `internal/update` holds Hole's own binary to. The
checksums must move with `COREDNS_VERSION`: a bump that forgets them fails the build rather than
installing an unverified tarball, and `TestGatewayDockerfileVerifiesCoreDNS` fails if the download
is ever piped straight into `tar` again.

Both image references carry a digest and **no tag**: the digest alone identifies the image, and a
tag beside it is redundant information that can drift out of sync with the digest it accompanies —
so the human-readable tag lives in the comment next to each constant instead.
`TestPinnedImagesCarryNoTag` and `TestGeneratedComposePinsSidecarImageByDigest` pin both halves.
Each digest was checked to be addressable as `…/manifests/sha256:<digest>` and to cover
linux/amd64 and linux/arm64 before being written down.

Refreshing a pin (from a host with a network and a container runtime):

```sh
# CoreDNS: bump the version, then take both sums from the release page
for arch in amd64 arm64; do
  curl -fsSL "https://github.com/coredns/coredns/releases/download/v${V}/coredns_${V}_linux_${arch}.tgz.sha256"
done

# Images: the multi-arch index digest, which must cover linux/amd64 and linux/arm64
docker buildx imagetools inspect docker:dind-rootless
docker buildx imagetools inspect registry:2
```

Consequences worth knowing before a bump:

- The gateway tag is derived from `assets.BuildInputsHash()`, so editing the Dockerfile
  invalidates every cached gateway image automatically — users get the verified build on their
  next start without asking for `-r`.
- Pinning `docker:dind-rootless` freezes the Docker Engine version inside sandboxes until the pin
  moves. Treat it as a maintenance item, not a set-and-forget.
- A new registry pin does **not** reach an existing mirror: `dindregistry.Ensure` restarts a
  stopped `hole-registry` rather than recreating it, to keep its cache volume. Remove the
  container to pick it up — `hole destroy` with no path (a full destroy takes the mirror too), or
  `docker rm -f hole-registry`. `Ensure` does remove a mirror that fails its readiness probe, but
  that is a crash-recovery path, not a way to pick up a pin: a mirror that starts fine keeps
  running whatever digest created it.

## Release pipeline

Push to `main` triggers `.github/workflows/release.yml`:

1. `codacy/git-version` resolves the next version from conventional commits — a `!` marker
   (`feat!:`, `refactor(gateway)!:`) or a `BREAKING CHANGE:` footer bumps the major, `feat:` and
   `feat(scope):` the minor, everything else the patch.

   How the action matches (from its source, `src/git-version.cr`) dictates how the identifiers must
   be written, and none of it is documented upstream:
   - It runs `git log --pretty=%B`, splits the output on newlines and tests **each line
     separately**, so a footer on its own line counts — but a line GitHub prefixes when
     squash-merging (`* feat!: …`) does not.
   - It **downcases** every line first. An uppercase `BREAKING CHANGE` in an identifier can
     therefore never match; write the pattern lowercase and it still catches the uppercase footer.
   - `major-identifier` is always compiled as a regex with `^` prepended. `minor-identifier` is a
     regex only when wrapped in `/…/`, and is left unanchored.
   - Both are single-quoted YAML, which processes no escapes, so their backslashes must stay
     **single**. A doubled one reaches the regex as a literal backslash and scoped commits fall
     through to a patch bump with nothing in the log to say so.

   A release that *should* be major only becomes one if a commit actually carries the marker. On a
   squash-merge that means the **squash subject** — `feat!:` on a branch commit alone is lost.
2. The resolved version becomes an annotated git tag and is pushed, because GoReleaser builds from
   tags and the repository should keep the annotation.
3. `release-drafter` publishes the release notes and **owns the release body**.
4. GoReleaser builds and uploads the binaries. It is handed the version explicitly through
   `GORELEASER_CURRENT_TAG` rather than inferring it, and `release.mode: keep-existing` stops it
   from replacing release-drafter's notes with its own (disabled) changelog.

Because the notes are published before the binaries are built, a build failure leaves a published
release with no assets — recoverable by re-running the job, but worth knowing.

`.goreleaser.yaml` produces `linux_amd64`, `linux_arm64` (which covers WSL), `darwin_amd64` and
`darwin_arm64` as **raw binaries** named `hole_<os>_<arch>`, plus `checksums.txt`. Raw rather than
archives so neither the installer nor self-update has an unpacking step.

`internal/update.BinaryAssetName` and the installer's asset name must stay in step with that
template; a unit test asserts both.

CI runs `goreleaser check` and `bash -n install.sh` on every PR, so a broken release config fails
there rather than after the release notes have already been published.

## Installation

`install.sh` (fetched from `main`, run via `curl | bash`) detects `uname -s`/`-m`, resolves the
latest release, downloads the binary and `checksums.txt`, verifies the sha256, and moves the binary
into `~/.local/bin/hole` in a single `mv`. An unverified binary is never written into place; that
check is what makes downloading an executable over the network defensible.

It also removes `~/.local/share/hole/` if a 1.x tarball install is still there.

### `go install`

`CGO_ENABLED=0 go install github.com/lukashornych/hole/v2/cmd/hole@latest` is the source alternative
to the installer, and works because the module has no non-Go build step: every runtime file is
`go:embed`-ed and the two dependencies are pure Go. `go.mod` declares `go 1.25`, which an older
toolchain satisfies by downloading `go1.25.x` on the spot unless `GOTOOLCHAIN=local` forbids it.

The `CGO_ENABLED=0` is not decoration. `make build` and GoReleaser both set it; `go install` instead
inherits the environment, and on a machine with a C toolchain the default `CGO_ENABLED=1` gives
`net` its cgo resolver — the resulting binary links against the system libc (`ldd` shows
`libc.so.6`) and stops being the portable static binary every release ships.

What it forfeits is the version stamp — but not its identity. See
[build identity](#build-identity) for what such a build may and may not do.

`go install` does accept `-ldflags` alongside a `pkg@version` query, so the stamp can be passed by
hand — and there is no reason to: it turns the build into a `Release` as far as Hole is concerned,
which means `hole update` will replace it in `GOBIN` with a downloaded release binary
(`resolveInstallPath` only resolves symlinks, it does not redirect to `~/.local/bin`). Leave it
unstamped and the identity is derived correctly on its own.

### Why the module path carries `/v2`

`go.mod` declares `module github.com/lukashornych/hole/v2`, and every internal import plus both
ldflags paths carry the suffix. Nothing imports Hole as a library, so the suffix exists for exactly
one reason: Go refuses to resolve a `v2.x` tag for a module path without it, and `+incompatible` is
unavailable because a `go.mod` exists. Without the suffix `@latest` resolves the newest `v1` tag —
the 1.x bash tree, which has no `cmd/hole` — and `@v2.0.0` is rejected outright, leaving branch and
commit queries as the only installable ones. The suffix stays for the whole 2.x line; a 3.0 would
have to move it to `/v3`.

Two consequences to keep in mind: `install.sh` and `hole update` are unaffected, because they
resolve GitHub *release assets* rather than module versions — and the version passed to `-ldflags`
must keep going into `…/v2/internal/version.Version`, or the stamp silently lands nowhere and every
release build reports `development`.

Nothing extra has to be published for a module version to exist: the proxy reads the repository's
**git tags**, and the release workflow already writes the form Go requires — `codacy/git-version`
runs with `prefix: 'v'`, so the tag is `v2.1.4`, at the repository root, on a commit whose `go.mod`
declares the `/v2` path. The GitHub release, its notes and its binaries are irrelevant to
`go install`; the tag alone is the module version, and `{{ .Version }}` in `.goreleaser.yaml` is the
same number without the `v`, which is what `Release.Version()` compares against.

Consequences of that coupling, both of them one-way:

- **A released tag must never move.** The proxy and `sum.golang.org` cache a version on first fetch
  and keep serving it even if the tag is deleted or repointed, so a moved tag means two different
  binaries under one version number. Withdraw a bad release by tagging a new patch and adding a
  `retract` directive in `go.mod`, not by re-tagging.
- **A tag whose major does not match the path is invisible**, so the day a `v3.0.0` is cut, `go.mod`
  and every import have to move to `/v3` in the same commit the tag points at — otherwise
  `@latest` silently keeps serving the newest 2.x.
- **The first tag of a major is what switches `go install` on.** Before any `v2.x` tag exists, *every*
  query fails — `@latest`, `@<branch>` and even an explicit pseudo-version — because Go resolves
  `@latest` for the module to look up its deprecation notice, and on a path with no matching versions
  that lookup is fatal: `loading deprecation for github.com/lukashornych/hole/v2: no matching
  versions for query "latest"`. Nothing is wrong with the module when this happens; it clears the
  moment the first `/v2` tag is published, which is also why a `/v3` rename cannot be validated by
  installing from a branch beforehand.

## Build identity

`internal/version` classifies the running binary into one of three kinds, because "stamped or not"
cannot answer the three questions Hole asks of its own version. A stamp wins outright; without one,
the **version-control settings** decide between the other two: a build from a working tree records
`vcs.revision`, and a module install — which builds from the module cache — records none.

That discriminator is not interchangeable with the module version, which is the trap here. Since Go
1.24 a build from a *clean* checkout also gets a module version, derived from the repository
(`v2.0.0-20260810132358-8014588bf523`), and only a dirty tree still falls back to `(devel)`. Reading
the module version alone would therefore classify `make build` on a clean checkout as a `go install`
build and let a working tree run migrations. `go version -m ./hole` shows the difference: both carry
a `mod` line, only the local build carries `vcs.*`.

| | `Release` (`-ldflags`) | `Source` (`go install`) | `Development` (`make build`) |
|---|---|---|---|
| `hole version` | `2.1.4` | `2.1.4 (go install)` | `development (bc2a181, dirty)` |
| `CheckForUpdate` | yes, offers `hole update` | yes, offers the `go install` command | no |
| `SelfUpdate` | yes | refuses, naming the `go install` command | refuses, naming the installer |
| `OnVersionChange` | yes | **yes** | no |

Why each answer is what it is:

- **Self-update is release-only.** Replacing a binary the user built from source with a downloaded
  one changes its provenance behind their back, inside a directory the Go toolchain owns. The hint
  they get instead is derived from the package path recorded in the binary, so it cannot drift from
  the module path — and when the newest release crossed a major version it names the `/vN` path,
  which `@latest` on the installed path would never reach.
- **Migrations are not release-only.** Someone arriving from 1.x via `go install` needs the same
  one-time cleanup as everyone else; gating it on the stamp meant they silently kept every 1.x
  image, volume and network. A checkout still skips it, because iterating on the code must not sweep
  Docker resources or rewrite `state.json`.
- **A pseudo-version is an identity but not a number.** `@main` and `@<sha>` installs resolve to
  `0.0.0-<ts>-<sha>`, which cannot be compared against a release, so `CanCompare` excludes them from
  the update notice while `CanMigrate` still lets them migrate.
- **The comparison tolerates a `v`.** `GreaterThan` strips a leading `v` and any pre-release suffix;
  without that, `2.0.0` compared as *newer* than the `v2.0.0` a tagged `go install` reports, and
  every run would announce an update to the version already installed. That case is a regression
  test.

`state.json` needs no special handling: a tagged source install records the same `2.1.4` a release
would, and it has already run the cleanup, so a later release install correctly skips it.

## Handing out a build before it is released

`go install …@<branch>` is not an option before the major's first tag exists (see above), so a
pre-release build reaches someone else one of two ways:

```sh
# 1. they build it themselves — needs a Go toolchain, reports `development (<sha>)`
git clone -b <branch> https://github.com/lukashornych/hole && cd hole
CGO_ENABLED=0 go install ./cmd/hole

# 2. you build it and hand over the binary — needs nothing on their side
VERSION=2.0.0-rc.1 make build
```

They differ in more than convenience. Route 1 produces a `Development` build: no update check, no
self-update, and **no version-change migration**, so a colleague coming from a 1.x install keeps
every 1.x image, volume and network. Route 2 produces a `Release` build that reports `2.0.0-rc.1`,
migrates like a real install, and can later `hole update` itself — which makes it the better way to
put a build in front of someone who is not developing Hole.

Route 2 has one wart worth knowing before you promise anything: `GreaterThan` compares numeric
components only, so `2.0.0` is *not* greater than `2.0.0-rc.1`. Someone on `2.0.0-rc.1` is told
"already up to date" until 2.0.1 ships, and has to install the final 2.0.0 the normal way.

## Self-update

`hole update` compares against the latest release, downloads and verifies the same way, then writes
the replacement next to the target and renames over it. That is atomic within a filesystem, so an
interrupted update cannot leave a half-written binary, and renaming over a running executable is
fine on Unix because the running process keeps its own inode.

It refuses to install anything when a release publishes no `checksums.txt`, and falls back to
printing the installer one-liner when replacement fails (a read-only install directory, or a
package-manager-owned path).

## Version-change migration

`~/.hole/state.json` records the last version that completed a run. On a change — including the
first run of the Go binary over a 1.x install — Hole removes what an older version left behind:
`hole-sandbox/{proxy,dns}-*` images and `:latest`-tagged agent images, the
`hole-sandbox-docker-cache` volume the pull-through registry replaced, 1.x agent-home volumes,
unattached 1.x networks, and the `~/.local/share/hole` directory. All logged, all best-effort.

Two details matter. Legacy networks are identified by **lacking** `hole.managed`, which is what
distinguishes them from a network the current version just created and has not attached anything to
yet. And the cleanup never touches `~/.local/bin/hole`: the 1.x wrapper lived there, and so does
the binary now.

## Uninstall

`hole uninstall` removes every Hole container, network, volume and image (including the registry
mirror), then the binary. `~/.hole` holds user data — settings, custom agents, logs — so it only
goes on an explicit yes; without a terminal the answer is no, because nobody was there to say
otherwise.

## Manual platform checklist

CI covers Linux with Docker only. These need checking by hand per release, because the gateway
depends on kernel and packaging details CI does not vary:

- **Docker Desktop, OrbStack, Colima (macOS)** and **WSL**: `hole start claude . -d`, then from
  inside the sandbox confirm an allowed domain resolves and an unlisted one gives NXDOMAIN.
- `nft -f` accepted the ruleset (a failure kills the gateway container, so startup aborts):
  `docker exec <instance>-gateway-1 nft list ruleset`.
- dnsmasq actually populates the sets: resolve an allowed name, then
  `nft list set inet hole g0`.
- `sysctls` and `cap_add` accepted by the runtime, and on rootless podman that `nft` may be loaded
  as the userns owner.
- Teardown leaves nothing: `hole list` empty, no `hole-sandbox-*` containers or networks.
