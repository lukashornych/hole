# Hole — Developer Documentation

Hole is a CLI tool for creating and managing Docker-based sandboxes for AI agents (Claude Code,
Gemini CLI, Codex CLI). It restricts an agent's network access via a filtering proxy with a domain
whitelist, controls file access via Docker volume mounts, and destroys the whole environment when
the agent exits.

This documentation is aimed at developers working on Hole itself. For user-facing documentation
(installation, usage, configuration reference), see the [README](../../README.md).

## Recommended reading order

1. [Architecture](architecture.md) — CLI entry point, sandbox lifecycle, compose file layering,
   startup/teardown sequences, security model
2. [Networking](networking.md) — proxy (tinyproxy) filtering, DNS (CoreDNS), whitelist merging,
   host gateway domains, network access logging
3. [Agents](agents.md) — the per-agent plugin structure, unified agent image, Dockerfile build
   phases, container user identity
4. [Configuration](configuration.md) — `settings.json` schema, validation, deep-merge semantics,
   and how every setting maps to Docker resources
5. [Guidelines](guidelines.md) — bash coding conventions, logging, error handling, documentation
   rules, git workflow
6. [Recipes](recipes.md) — step-by-step instructions: add a new agent, add a settings option,
   test from a dev checkout, debug a sandbox, pre-PR checklist
7. [Build & Release](build-and-release.md) — install/update/uninstall mechanics, release
   workflow, versioning

## Repository layout

```
hole.sh                   # main CLI (start/destroy/update/uninstall/version/help)
logger.sh                 # sourced logging library (log_info/log_warn/log_error/log_line)
utils.sh                  # sourced helpers (require_cmd)
install.sh                # standalone installer (downloads latest GitHub release)
uninstall.sh              # standalone uninstaller (also used by `hole update`)
docker-compose.yml        # base compose file — defines the two networks only
schema/
  settings.schema.json    # JSON Schema for global + project settings.json
agents/
  Dockerfile              # unified agent image (Ubuntu 24.04 + all enabled agent CLIs)
  docker-compose.yml      # agent service definition
  entrypoint.sh           # container entrypoint (prestart hooks, then exec agent CLI)
  <agent>/                # per-agent plugin dir (claude/, gemini/, codex/)
    install-root.sh       #   optional root-phase install script
    install-user.sh       #   optional user-phase install script
    command.json          #   startup command (JSON array of argv parts)
    allowed-domains.txt   #   agent-specific domain whitelist (tinyproxy regex)
proxy/
  Dockerfile              # Alpine + tinyproxy
  docker-compose.yml      # proxy service definition
  tinyproxy.conf          # filtering config (whitelist enforced)
  tinyproxy-unrestricted.conf  # config for -u/--unrestricted-network
  allowed-domains.txt     # default (empty) shared whitelist base
dns/
  Dockerfile              # Alpine + CoreDNS (downloaded release binary)
  docker-compose.yml      # dns service definition
  Corefile                # default forward-only CoreDNS config
  entrypoint.sh           # substitutes host gateway IP into Corefile template
documentation/developer/  # this documentation
.github/workflows/release.yml  # release packaging + publishing (push to main)
.github/release-drafter.yml    # release notes template
```

## Documentation rules

- **User-facing** changes (CLI flags, settings, behavior) must be documented in
  [README.md](../../README.md).
- **Developer-facing** changes (architecture, internals, conventions) must be reflected in these
  `documentation/developer/` files.
- New source files must be added to `.github/workflows/release.yml`
  (see [recipes](recipes.md#add-a-new-source-file)).
