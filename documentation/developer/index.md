# Hole — Developer Documentation

Hole is a CLI tool for creating and managing Docker-based sandboxes for AI agents (Claude Code,
Gemini CLI, Codex CLI, and user-defined agents). It denies the sandbox all network access except
what you allow — on every protocol and port — controls file access via Docker volume mounts, and
destroys the whole environment when the agent exits.

This documentation is aimed at developers working on Hole itself. For user-facing documentation
(installation, usage, configuration reference), see the [README](../../README.md); for upgrading
from 1.x, [MIGRATION.md](../../MIGRATION.md).

## Recommended reading order

0. [Development environment](development.md) — prerequisites, first-time setup, operating the four
   test suites, golden files, matching CI locally
1. [Architecture](architecture.md) — package layout, sandbox identity, startup and teardown, the
   watchdog and garbage collection, security model
2. [Networking](networking.md) — the filtering gateway, the allow-list model, generated artifacts,
   subnet allocation, accepted limitations
3. [Agents](agents.md) — the plugin contract, builtin and user agents, the unified image build
4. [Configuration](configuration.md) — settings loading and validation, merge semantics, profiles,
   image identity and scope, compose generation
5. [Guidelines](guidelines.md) — toolchain, Go conventions, non-negotiable rules, git workflow
6. [Recipes](recipes.md) — add an agent or a setting, debug a sandbox, pre-PR checklist
7. [Build & release](build-and-release.md) — versioning, GoReleaser, installation (`install.sh` and
   `go install`), build identity, self-update, uninstall, manual platform checklist

## Repository layout

```
cmd/hole/                 CLI entry point
internal/                 implementation packages (see architecture.md)
assets/                   go:embed root: agent plugins, Dockerfiles, entrypoints, JSON schema
test/e2e/                 end-to-end suite
Makefile                  build, test, itest, e2e, lint, golden (see development.md)
install.sh                standalone installer (downloads the release binary, verifies it)
.goreleaser.yaml          release build configuration
.github/workflows/ci.yml       lint + unit (linux, macos) + integration + e2e + podman parity
.github/workflows/release.yml  version resolution, tagging, release notes, GoReleaser
documentation/developer/  this documentation
documentation/analysis/   design analyses and the rewrite plan this implementation followed
```

## Documentation rules

- **User-facing** changes (CLI flags, settings, behavior) must be documented in
  [README.md](../../README.md), and in [MIGRATION.md](../../MIGRATION.md) when they change 1.x
  behavior.
- **Developer-facing** changes (architecture, internals, conventions) must be reflected in these
  files, in the same change as the code.
- New runtime files must live under `assets/` beneath an embed directive — see
  [guidelines](guidelines.md#non-negotiable-rules).
