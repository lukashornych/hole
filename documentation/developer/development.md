# Development Environment

How to set up a machine for working on Hole, and how to operate the four test suites. For the
conventions the code must follow, see [guidelines](guidelines.md); for task-shaped walkthroughs
(add an agent, add a setting, debug a sandbox), see [recipes](recipes.md).

## Prerequisites

| Tool | Needed for | Notes |
|---|---|---|
| **Go 1.25+** | everything | `go.mod` declares `go 1.25` and pins no `toolchain`, so any newer release works. Builds are `CGO_ENABLED=0`, so no C compiler is required. |
| **make** | the documented entry points | Every target is a one-line `go` invocation; you can run them by hand if you prefer. |
| **git** | the checkout, and part of the unit suite | `internal/worktree` shells out to `git`; its tests skip without it. |
| **docker or podman + the compose plugin** | `make itest`, `make e2e`, manual runs | Hole's own runtime requirement, see the [README](../../README.md#installation). `engine.Detect` verifies `<binary> compose version` works, not just that the binary exists. |
| **golangci-lint** | optional, part of `make lint` | The repo ships **no** `.golangci.yml`, so the linter runs on its defaults. CI uses `version: latest`; a pinned older local install can therefore disagree with CI. |
| **goreleaser** | optional | Only to reproduce the `release-config` CI job locally — see [build & release](build-and-release.md). |

Dependencies of the Go module itself are deliberately two — `gopkg.in/yaml.v3` and
`github.com/santhosh-tekuri/jsonschema/v6`. Adding a third needs a justification; see
[guidelines § Toolchain](guidelines.md#toolchain).

## First-time setup

```sh
git clone https://github.com/lukashornych/hole.git
cd hole
go mod download          # two direct modules, seconds
make build               # static binary at ./hole
./hole version           # prints "hole development"
```

`./hole` is git-ignored. For what a `development` build does differently and how to run a sandbox
from a checkout, see [recipes](recipes.md#build-and-run-from-a-checkout).

Everything Hole writes at runtime lives under `~/.hole` (state, logs, user agents, run temp dirs), so
a development build and an installed one share that directory, and every command acts on the host
globally. Read the warning under [Integration](#integration--make-itest) before running `make itest`
on a machine where you use Hole for real work.

## The four suites

```sh
make test     # unit — no container runtime needed, seconds
make itest    # integration — needs a real docker/podman daemon (see the warning below)
make e2e      # end-to-end — real sandboxes, builds images, slow
make lint     # gofmt + go vet under all build tags + golangci-lint if installed

make all      # lint + test + build, the usual pre-push sweep
make clean    # remove ./hole
```

### Unit — `make test`

`go test ./...` — everything without a build tag. Covers merge semantics and profiles, the path
resolution pipeline, the glob matcher, the allow-list grammar, the subnet allocator, compose and
gateway golden files, image hashing and scope, and CLI parsing. No daemon, no network.

**CI runs `go test -race ./...`, the Makefile does not.** Local green is not CI green for anything
touching the watchdog, the state lock, or concurrent allocation. Before pushing:

```sh
go test -race ./...
```

CI runs the unit job on both `ubuntu-latest` and `macos-latest`.

### Integration — `make itest`

`go test -tags integration -count=1 -timeout 20m -p 1 ./...`. Four packages carry
`//go:build integration` files: `internal/engine`, `internal/sandbox`, `internal/dindregistry`,
`internal/update`. They exercise real network/volume/container operations, a 12-way concurrent
subnet allocation race, every GC pass including its keep-conditions, teardown completeness and
idempotence, the registry mirror lifecycle, and the version-change migration. They fabricate the
resources a sandbox would own instead of building images, so they finish in seconds.

The one exception is `internal/sandbox/gateway_integration_test.go`, which builds the real gateway
image and runs its entrypoint against a fabricated `/etc/hosts` — the only way to reproduce how a
runtime hands over the host gateway address. It reuses an already-built image and otherwise needs
network access for the build (GitHub for CoreDNS, the Ubuntu mirrors for packages); without it the
two cases skip with a message naming the build, which is what happens inside the agent sandbox. The
builder cache survives the uninstall test's image sweep, so a rebuild costs seconds. The generated
configuration is `docker cp`'d in rather than bind-mounted, so the cases also work against a remote
daemon.

Three traps:

- **The suite destroys Hole resources on the host daemon.**
  `TestUninstallKeepsUserDataUnlessAsked` in `internal/update` calls the production `Uninstall`,
  which removes *every* `hole-sandbox-*` container, network and volume, every `hole-sandbox/*`
  image and the registry mirror — not just the ones the test created. Your `~/.hole` is safe (the
  test uses a temporary `HOME`) and so is your binary (`KeepBinary: true`), but **a sandbox you had
  running will be killed and your cached sandbox images will be gone**, so the next real run
  rebuilds from scratch. Run `hole list` first, or run the suite on a machine you are not using
  Hole on.
- **`-p 1` is mandatory.** The packages share one daemon and globally-named resources — the image
  mirror, the `hole-sandbox-*` namespace — and, per the point above, one of them deliberately
  wipes that namespace. A package-parallel run has one package deleting another's fixtures. Run
  `make itest`, not `go test -tags integration ./...`.
- **The suite passes vacuously without a daemon.** Every integration package obtains its engine
  through a helper that does `t.Skipf("no container runtime available: %v", err)`, so a machine
  with no docker/podman prints `ok` for all packages and proves nothing. Check for that specific
  string:

  ```sh
  go test -tags integration -count=1 -p 1 -v ./... 2>&1 | grep -c "no container runtime available"
  ```

  Nonzero means your runtime was not detected and nothing was really tested. (Other skips are
  normal — individual cases skip when a fixture container will not start.)

### End-to-end — `make e2e`

`go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/`. Full start → attach → exit →
zero-leftovers runs, plus default-deny versus allowed domains, the `-n` domain dump, exclusion
hiding, hooks, `destroy`, and the watchdog matrix (SIGINT, `kill -9` of the CLI, `kill -9` of the
watchdog, `kill -9` of both).

**Budget 10–20 minutes** with a warm image cache, more on a cold one (the `ubuntu:24.04` pull and
two image builds). The tests are strictly sequential — no `t.Parallel()` — and each one is a real
sandbox start and teardown, roughly 30–70 seconds apiece. The 60-minute timeout is a ceiling for a
pathological run, not an estimate. `-v` is not optional comfort: `go test` buffers a package's
output until the package finishes, so without it a quarter-hour run is silent and
indistinguishable from a hang.

- **No API key and no real agent CLI are needed.** `TestMain` builds the binary itself into a temp
  directory, and each test writes a *test agent* — a user agent plugin under a temporary `HOME`
  whose `command.json` is a shell one-liner. Installing the real agent CLIs would make every run
  download Node and three vendor installers.
- **It does need docker specifically.** The assertions shell out to `docker` directly
  (`docker ps`, `docker network ls`, `docker volume ls`), so `HOLE_RUNTIME=podman` does not carry
  the e2e suite.
- **The isolated `HOME` keeps your real docker client configuration.** Each `hole` child process
  runs with `HOME` pointed at a temp directory, but the harness also passes `DOCKER_CONFIG` for the
  real `~/.docker` unless you have already set it, because that is where the docker CLI finds its
  cli-plugins and contexts — on macOS the compose plugin lives there, so without it every test
  fails at `'docker compose' is not available`. `TestMain` probes this before running anything and
  aborts the suite immediately if it fails; `TestDockerClientIsUsableUnderTheTestEnvironment` is
  the regression test for the same condition.
- **It does need the internet on a cold cache**: the sandbox image builds `FROM ubuntu:24.04`, the
  gateway on the same base, and the registry mirror pulls `registry:2`. First run is slow; later
  runs hit the local image cache.
- Unlike the integration suite, e2e does **not** skip when the runtime is missing — it fails.

**Run it on the host, not inside an agent sandbox or any other remote-daemon setup.** Two things
break there: image builds have no DNS (`apt-get` cannot reach `ports.ubuntu.com`, `curl` cannot
reach `github.com` for CoreDNS), and bind mounts resolve on the *daemon's* filesystem, so the
gateway's three config files and the project directory arrive as empty directories and the gateway
never starts. On the host it is safe in the ways that matter — `~/.hole` and your binary are
untouched (temporary `HOME`, no `Uninstall`, and `hole destroy` is only ever called with a path, so
it stays project-scoped) — with one caveat: its GC runs under that temporary `HOME` and so cannot
recognise your real instances, meaning a *half-dead* sandbox of yours (stopped containers, networks
past the 10-minute grace period) can be swept along with its Docker-in-Docker volume, though a
running one is protected by the `anyRunning` check. Run `hole list` first.

### Lint — `make lint`

```sh
gofmt -l cmd internal assets test     # must print nothing
go vet ./...
go vet -tags integration ./...
go vet -tags e2e ./...
CGO_ENABLED=0 GOOS=linux go build ./...    # both shipped platforms
CGO_ENABLED=0 GOOS=darwin go build ./...
golangci-lint run                     # skipped with a notice if not installed
```

The three `go vet` invocations are not redundant: plain `go vet ./...` never compiles the
build-tagged test files, so a broken integration or e2e test survives it. The two cross-compiles
are there because `internal/engine`'s terminal check is GOOS-specific (`tty_linux.go`,
`tty_darwin.go`), so a host-only build would not notice a break on the other platform until
GoReleaser hit it at release time. `make fmt` rewrites files in place.

## Golden files

Generated artifacts are asserted against checked-in golden files in
`internal/network/testdata` (gateway Corefile, dnsmasq and nftables configuration) and
`internal/sandbox/testdata` (compose files). After an intended change:

```sh
make golden        # go test ./internal/network/ ./internal/sandbox/ -update -count=1
```

Read the resulting diff — that is the entire point of golden files — and commit it with the change.

`make golden` names those two packages explicitly, matching the `-update` flags declared in
`internal/network/gateway_test.go` and `internal/sandbox/composegen_test.go`. If you add golden
coverage to a third package, extend the Makefile target too, or its goldens will silently never
refresh.

## Environment variables

- **`HOLE_RUNTIME`** — forces the container runtime (`docker` or `podman`) instead of the
  docker-then-podman probe in `engine.Detect`. It is a user-facing variable, documented in the
  [README](../../README.md); in development it is how you run the integration suite against podman:

  ```sh
  HOLE_RUNTIME=podman go test -tags integration -count=1 -timeout 20m -p 1 ./internal/engine/
  ```

  CI has a podman job that does exactly this, marked `continue-on-error: true` — podman parity is
  tracked, not gated.
- `HOLE_TEST_LIVENESS_DIR` / `HOLE_TEST_LIVENESS_NAME` are internal: `internal/state` re-execs its
  own test binary as a lock-holding helper through them. Never set them by hand.

## Matching CI locally

CI (`.github/workflows/ci.yml`) has six jobs. Their local equivalents:

| CI job | Local command |
|---|---|
| Lint | `make lint` |
| Unit (ubuntu, macos) | `go test -race ./...` |
| Integration (docker) | `make itest` |
| E2E (docker) | `make e2e` |
| Podman parity (non-blocking) | `HOLE_RUNTIME=podman go test -tags integration -count=1 -p 1 ./internal/engine/` |
| Release config | `goreleaser check` and `bash -n install.sh` |

The `release-config` job is the one developers most often forget — why it exists is covered in
[build & release](build-and-release.md).

## Troubleshooting the suites

| Symptom | Cause |
|---|---|
| `make itest` finishes instantly, all `ok` | no runtime detected — every package skipped; check `docker version` and `docker compose version` |
| `'docker compose' is not available` | the compose plugin is missing; `engine.Detect` requires the subcommand, not just the binary |
| Integration tests fail on leftover resources | a previous run aborted; `./hole destroy` removes all Hole Docker resources (your own sandboxes included) without touching `~/.hole` or the binary, then re-run |
| `make e2e` times out on a cold machine | the first run pulls `ubuntu:24.04` and `registry:2` and builds two images; the 60m timeout is sized for that |
| `make e2e` aborts at once with `docker compose is not usable under the test environment` | the `TestMain` precheck: the docker CLI cannot see its compose plugin under the harness environment — the message names the `DOCKER_CONFIG` it used, which must contain `cli-plugins/docker-compose` or resolve one from a system directory |
| `sandbox left containers behind` naming an instance from an earlier run | orphans from a previously aborted run; the assertion prints resource *names*, so compare the instance ID in the name against the run you just did, then clear them with `./hole destroy` |
| Golden test fails after an unrelated change | map iteration reached generated output unsorted — see `config.SortedKeys` in [guidelines](guidelines.md#go-conventions) |
| `go vet` clean locally, CI lint red | you ran `go vet ./...` only; CI also vets the `integration` and `e2e` tags |
| `golangci-lint` clean locally, CI lint red | there is no `.golangci.yml`; CI uses `latest` and your local install may be older |

For debugging an actual sandbox run rather than the test suites, see
[recipes § Debug a failing sandbox](recipes.md#debug-a-failing-sandbox).

## Before opening a PR

See the [pre-PR checklist](recipes.md#pre-pr-checklist).
