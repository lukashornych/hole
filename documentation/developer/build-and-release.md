# Build & release

## Local build

```sh
make build                  # ./hole, static, version "development"
VERSION=2.0.0 make build    # stamped, as a release build is
```

The version is injected with `-ldflags -X github.com/lukashornych/hole/internal/version.Version`.
An unstamped build reports `development`, skips update checks and refuses to self-update — so a
checkout can never accidentally overwrite itself with a release.

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
  `docker rm -f hole-registry`.

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

CI covers Linux with Docker, plus a rootless-podman parity job for the engine call sites. These
need checking by hand per release, because the gateway depends on kernel and packaging details CI
cannot vary:

- **Docker Desktop, OrbStack, Colima (macOS)** and **WSL**: `hole start claude . -d`, then from
  inside the sandbox confirm an allowed domain resolves and an unlisted one gives NXDOMAIN.
- `nft -f` accepted the ruleset (a failure kills the gateway container, so startup aborts):
  `docker exec <instance>-gateway-1 nft list ruleset`.
- dnsmasq actually populates the sets: resolve an allowed name, then
  `nft list set inet hole g0`.
- `sysctls` and `cap_add` accepted by the runtime, and on rootless podman that `nft` may be loaded
  as the userns owner.
- Teardown leaves nothing: `hole list` empty, no `hole-sandbox-*` containers or networks.
