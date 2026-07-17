# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hole is a CLI tool for creating and managing Docker-based sandboxes for AI agents (Claude Code,
Gemini CLI, Codex CLI). It provides:
- Network access control via a filtering proxy with a domain whitelist
- File access control via Docker volume mounts
- An isolated execution environment that is destroyed when the agent exits

All enabled agents are installed into a single unified sandbox image.

Technology stack: Bash, Docker Compose (docker or podman), tinyproxy, CoreDNS, jq, jv (JSON Schema validation).

## Documentation

Developer documentation lives in `documentation/developer/` — **read it before analyzing source code**;
it covers the architecture, networking, agent system, configuration mechanics and step-by-step recipes:

- [index](documentation/developer/index.md) — TOC with recommended reading order and repository layout
- [architecture](documentation/developer/architecture.md) — CLI entry point, sandbox lifecycle, compose layering, startup/teardown sequences, security model
- [networking](documentation/developer/networking.md) — proxy filtering, whitelist merging, DNS, host gateway domains, network access logging
- [agents](documentation/developer/agents.md) — per-agent plugin structure, unified image build phases, container user identity
- [configuration](documentation/developer/configuration.md) — `settings.json` schema, validation, merge semantics, how each setting maps to Docker resources
- [guidelines](documentation/developer/guidelines.md) — bash conventions, error handling, git workflow
- [recipes](documentation/developer/recipes.md) — add a new agent / settings option / source file, debugging, pre-PR checklist
- [build & release](documentation/developer/build-and-release.md) — install/update/uninstall mechanics, release workflow, versioning

When implementing or changing any functionality, it HAS TO BE REFLECTED in the documentation files:
user-facing changes (CLI flags, settings, behavior) in `README.md`, developer-facing changes in
`documentation/developer/`.

### Source code pollution

Don't overcomment the source code itself; focus documentation into `README.md` and
`documentation/developer/`. If you comment code, keep only what the code cannot express
(constraints, non-obvious reasons) — never implementation-plan content like phases or task lists.

## Code guidelines

- use shell strict mode (`set -euo pipefail`)
- use local variables in functions
- do NOT use global variables to pass data between functions
- always double-quote variables
- prefer `${VAR}` syntax
- use lowercase for local variables
- use `$()` for command substitution
- use `[[ ]]` for conditionals
- use arithmetic expansion `(( ))` for math
- use `getopts` for command-line argument parsing
- log using the sourced `logger.sh` library (`log_info`, `log_error`, `log_warn`, `log_line`); do not use `echo` for logging
- error handling: `log_warn` + skip for ignorable user-config problems; `log_error` + `exit 1` for anything that makes the sandbox wrong or unsafe (details: [guidelines](documentation/developer/guidelines.md#error-handling))

## Running & verifying

There is no build step or test suite — run the CLI directly from the checkout and verify changes manually:

```bash
./hole.sh start claude /path/to/project      # full sandbox run
./hole.sh start claude . -d                  # bash shell instead of agent CLI (inspect sandbox)
./hole.sh start claude . -r                  # force image rebuild
./hole.sh start claude . -n                  # dump ALLOWED/DENIED domains on exit
```

Requires docker/podman + compose, `jq`, `jv`. Debugging tips: [recipes](documentation/developer/recipes.md#debug-a-failing-sandbox).

## Planning

When asked for issue analysis, always create a detailed implementation or fix plan and store it
in a Markdown file inside this project.

## Non-negotiable rules

- New source files MUST be added to the packaging step in `.github/workflows/release.yml`,
  otherwise they will be missing from installed releases.
- New or changed settings MUST be added to `schema/settings.schema.json` — the schema is strict
  (`additionalProperties: false`), so unlisted options break every user's startup validation.
- Path-valued settings go through the shared resolution pipeline (`expand_env_vars` →
  tilde → relative-to-project); don't hand-roll path handling.
- Cleanup/teardown code is best-effort: never abort, `|| true` + warnings only.

## Git Workflow

- `main`: released versions only — every push to `main` triggers the release workflow
- `dev`: current development (target for PRs and feature branches)
- Feature branches: created from `dev`
- Use [conventional commits](https://www.conventionalcommits.org/) — release versioning depends
  on this (`feat:` bumps minor, everything else patch)

## External Documentation

Always use Context7 MCP when you need library/API documentation, setup or configuration steps
without being explicitly asked.
