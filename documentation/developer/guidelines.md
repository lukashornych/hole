# Guidelines

## Toolchain

Go 1.27+, module `github.com/lukashornych/hole/v2` (the suffix is required for a 2.x module and is
what makes `go install` work — see
[build & release](build-and-release.md#why-the-module-path-carries-v2)), `CGO_ENABLED=0`.

Keep the number of libraries small. This is a principle, not a fixed list: before adding a
dependency, decide whether the feature really needs a whole library or whether a small utility of
our own does the job. A library is worth it when the problem is large or full of edge cases
(a YAML emitter, a JSON Schema engine, a terminal UI); it is not when we would use a sliver of it
that a few dozen to a hundred lines of standard library cover. Hole ships as one small static
binary, and its runtime requirements for users are deliberately just docker or podman. Prefer
pure-Go libraries with a small dependency tree and pin a release at least two weeks old.

Current libraries:

- `gopkg.in/yaml.v3` — compose file generation
- `github.com/santhosh-tekuri/jsonschema/v6` — settings validation (the engine behind `jv`)
- `github.com/rivo/tview` on `github.com/gdamore/tcell/v2` — the `hole modify … edit` terminal UI;
  imported only by `internal/modifyui`, so the rest of the code never depends on it

## Deviations from the rewrite plan

Two, both documented where they are implemented:

- **The gateway image is tagged by content, not `:latest`** — see
  [configuration](configuration.md#image-identity-and-scope). Compose never rebuilds an existing
  tag, so a fixed tag makes a shipped gateway fix unreachable.
- **Self-update is hand-rolled** rather than using `go-selfupdate`, for the dependency reason
  below.

> The rewrite plan named `github.com/creativeprojects/go-selfupdate` for self-update. Its current
> release pulls in ~50 modules — CEL, antlr, protovalidate, the Gitea SDK, dbus, wincred — for
> three things that are a hundred lines of standard library in `internal/update`. That is the kind
> of trade this principle exists to prevent.

## Go conventions

- `gofmt` (enforced in CI) and `go vet` under **all** build tags — `go vet ./...` alone misses the
  `integration` and `e2e` files.
- Exported identifiers carry doc comments. Comments explain constraints and non-obvious reasons —
  why an flock rather than a PID check, why `$$` in compose values — never what the next line
  plainly does, and never implementation-plan content like phases or task lists.
- Errors: wrap with `%w` and context (`fmt.Errorf("read %s: %w", path, err)`). Map the
  error-handling policy as `logging.Warn` + skip for ignorable user-config problems, a returned
  `error` for anything fatal, and log-only best effort in teardown. See
  [configuration](configuration.md#error-handling-policy).
- Local variable names are meaningful (`containerEngine`, not `e`); receiver names are short.
- Every docker/podman invocation goes in `internal/engine`. No exceptions — that is what makes
  podman parity testable in one place.
- Map iteration must be sorted before it reaches generated output (`config.SortedKeys`), so
  compose files and image hashes are reproducible.
- Prefer table-driven tests. Golden files for generated artifacts, refreshed with `make golden`
  and reviewed as part of the diff.

## Non-negotiable rules

- **New runtime files must live under `assets/`**, beneath a directory covered by an embed
  directive in `assets/assets.go`. A missing file then fails the build loudly, instead of vanishing
  from a release artifact.
- **New or changed settings must be added to `assets/schema/settings.schema.json`.** The schema is
  strict (`unevaluatedProperties: false` at the root and in every profile), so an unlisted option
  breaks every user's startup validation.
- **Path-valued settings go through the shared resolution pipeline**
  (`hostenv.Host.ResolveHostPath` / `ResolveContainerPath`). Don't hand-roll path handling.
- **Teardown and cleanup code never aborts**: best-effort, warnings only, and name every leftover
  it could not remove.
- **`hole list --json` fields are a public contract** consumed by the JetBrains plugin; only add
  fields, through `listEntry` (see
  [architecture](architecture.md#hole-list---json-is-a-public-contract)).

## Documentation rules

- **User-facing changes** (CLI flags, settings, observable behavior) → [README](../../README.md),
  and [MIGRATION.md](../../MIGRATION.md) when 1.x behavior changes.
- **Developer-facing changes** (architecture, internals, conventions) → these files, in the same
  change as the code.
- Keep source comments to what the code cannot express. Plans live in their own Markdown files
  under `documentation/plans/` (git-ignored, never committed); reasoning that has to outlive a
  change goes into a [decision record](../adr/README.md).

## Git workflow

- `main`: release branch — every push triggers the release workflow
  (see [build & release](build-and-release.md)).
- `dev`: current development; the target for PRs and feature branches.
- Feature branches are created from `dev`.
- Use [conventional commits](https://www.conventionalcommits.org/). Versioning depends on it: the
  release workflow's `codacy/git-version` bumps the **minor** version on `feat:` commits and the
  patch version otherwise.
