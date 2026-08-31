# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hole is a CLI tool for creating and managing Docker-based sandboxes for AI agents (Claude Code,
Gemini CLI, Codex CLI, plus user-defined agents). It provides:
- Network access control via a filtering gateway that denies everything not explicitly allowed,
  on every protocol and port
- File access control via Docker volume mounts
- An isolated execution environment that is destroyed when the agent exits

All enabled agents are installed into a single unified sandbox image.

Technology stack: Go 1.25 (single static binary, `CGO_ENABLED=0`), Docker Compose (docker or
podman), CoreDNS + dnsmasq + nftables inside the gateway container. Two Go libraries:
`gopkg.in/yaml.v3` and `github.com/santhosh-tekuri/jsonschema/v6`. Everything else is stdlib.

## Documentation

Developer documentation lives in `documentation/developer/` — **read it before analyzing source
code**; it covers the architecture, networking, agent system, configuration mechanics and
step-by-step recipes:

- [index](documentation/developer/index.md) — TOC with recommended reading order and repository layout
- [development](documentation/developer/development.md) — prerequisites, first-time setup, the four test suites, golden files, env vars, matching CI locally
- [architecture](documentation/developer/architecture.md) — package layout, sandbox identity, startup/teardown, watchdog, GC, security model
- [networking](documentation/developer/networking.md) — the filtering gateway, allow-list model, generated artifacts, subnet allocation, limitations
- [agents](documentation/developer/agents.md) — plugin contract, builtin and user agents, unified image build
- [configuration](documentation/developer/configuration.md) — `settings.json` schema, validation, merge semantics, profiles, image identity and scope
- [guidelines](documentation/developer/guidelines.md) — Go conventions, error handling, non-negotiable rules, git workflow
- [recipes](documentation/developer/recipes.md) — add an agent / setting / asset, debug a sandbox, pre-PR checklist
- [build & release](documentation/developer/build-and-release.md) — versioning, GoReleaser, installation, self-update, uninstall, manual platform checklist

When implementing or changing any functionality, it HAS TO BE REFLECTED in the documentation files:
user-facing changes (CLI flags, settings, behavior) in `README.md` — and in `MIGRATION.md` when
they change 1.x behavior — developer-facing changes in `documentation/developer/`.

### Source code pollution

Don't overcomment the source code itself; focus documentation into `README.md` and
`documentation/developer/`. If you comment code, keep only what the code cannot express
(constraints, non-obvious reasons) — never implementation-plan content like phases or task lists.

## Code guidelines

- `gofmt` everything; run `go vet` under **all** build tags (`./...`, `-tags integration`,
  `-tags e2e`) — plain `go vet ./...` misses the tagged test files
- doc comments on exported identifiers; comments explain constraints and non-obvious reasons, not
  what the next line plainly does
- wrap errors with `%w` and context: `fmt.Errorf("read %s: %w", path, err)`
- meaningful local names (`containerEngine`, not `e`); short receiver names
- every docker/podman invocation goes in `internal/engine` — no exceptions
- sort map iteration before it reaches generated output (`config.SortedKeys`), so compose files and
  image hashes are reproducible
- prefer table-driven tests; golden files for generated artifacts, refreshed with `make golden`
- error handling: `logging.Warn` + skip for ignorable user-config problems (missing excluded path,
  glob with no matches, missing hook script, undefined env var); returned `error` for anything that
  makes the sandbox wrong or unsafe (schema violation, unknown agent/profile, invalid allow entry,
  failed network allocation); teardown never aborts
  (details: [guidelines](documentation/developer/guidelines.md))

## Running & verifying

```bash
make build     # static binary at ./hole
make test      # unit tests, no container runtime needed
make itest     # integration tests, needs a real docker/podman daemon
make e2e       # end-to-end sandbox runs with the generated test agent (slow)
make lint      # gofmt + go vet (all build tags) + golangci-lint if installed
make golden    # regenerate golden compose/gateway artifacts after an intended change
```

**Always run `make test` and `make itest` yourself** — both work inside the agent sandbox (the
integration suite reaches the daemon over `DOCKER_HOST` and needs no bind mounts), and `make itest`
is only destructive to Hole's own Docker resources, which the sandbox has none of.

**Never run `make e2e` yourself** — it cannot pass inside the sandbox: image builds have no DNS to
`ports.ubuntu.com`/`github.com`, and bind mounts silently resolve to empty directories on the
remote daemon, so the gateway never starts. Ask the developer to run it on the host instead, and
tell them it leaves `~/.hole` and their binary untouched but shares the subnet pool and image cache
and — because its GC runs under a temporary `HOME` that does not know their real instances — can
sweep a *half-dead* sandbox of theirs (stopped containers, networks older than 10 minutes), though
never a running one.

Manual checks from a checkout:

```bash
./hole start claude /path/to/project      # full sandbox run
./hole start claude . -d                  # bash shell instead of agent CLI (inspect sandbox)
./hole start claude . -r                  # force image rebuild
./hole start claude . -n                  # dump resolved/refused domains on exit
./hole list                               # running sandboxes
```

Requires docker/podman + the compose plugin. Debugging tips:
[recipes](documentation/developer/recipes.md#debug-a-failing-sandbox).

## Planning

When asked for issue analysis, always create a detailed implementation or fix plan and store it
in a Markdown file inside this project (`documentation/analysis/`).

## Non-negotiable rules

- New runtime files MUST live under `assets/`, beneath a directory covered by an existing
  `go:embed` directive in `assets/assets.go`. A missing file then fails the build loudly instead of
  vanishing from a release artifact.
- New or changed settings MUST be added to `assets/schema/settings.schema.json`, inside
  `$defs/settings` so profiles get them too. The schema is strict
  (`unevaluatedProperties: false`), so an unlisted option breaks every user's startup validation.
- Settings that affect image content MUST also be added to the canonical config in
  `internal/image` and to the classification table in
  [configuration](documentation/developer/configuration.md#image-identity-and-scope), or cached
  images go stale.
- Path-valued settings go through the shared resolution pipeline
  (`hostenv.Host.ResolveHostPath` / `ResolveContainerPath`); don't hand-roll path handling.
- Cleanup/teardown code is best-effort: never abort, warnings only, and name every leftover.

## Git Workflow

- `main`: released versions only — every push to `main` triggers the release workflow
- `dev`: current development (target for PRs and feature branches)
- Feature branches: created from `dev`
- Use [conventional commits](https://www.conventionalcommits.org/) — release versioning depends
  on this (`feat:` bumps minor, everything else patch)

## External Documentation

Always use Context7 MCP when you need library/API documentation, setup or configuration steps
without being explicitly asked.
