# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hole is a CLI tool for creating and managing sandboxes for AI agents. It provides:
- Network access control via proxy with domain whitelist
- File access control via Docker volume mounts
- Isolated execution environment using Docker containers

Currently supports Claude Code agent (with placeholder for Gemini agent).

## Code guidelines

- use local variables in functions
- do NOT use global variables to pass data between functions
- use shell strict mode
- always Double-Quote Variables
- prefer ${VAR} Syntax
- use Lowercase for Local Variables
- use $() for Command Substitution
- use [[ ]] for Conditionals
- use Arithmetic Expansion (( )) for math
- use `getopts` for command-line argument parsing
- log using sourced logger.sh library (log_info, log_error, log_warn, log_line), do not use echo for logging

## Documentation

If the implemented feature affects user configuration or behavior, document it in the README.md file.

## Architecture

The project uses Docker Compose to orchestrate a multi-container sandbox environment:

### Container Architecture

**Two-network design for security:**
- `sandbox` network: Internal network (no direct internet access) where agents run
- `internet` network: Bridge network that only the proxy can access

**Two main services:**
- `proxy`: Tinyproxy-based HTTP/HTTPS proxy that filters requests to allowed domains only (proxy/allowed-domains.txt)
- `{agent}`: agent container e.g.: Claude Code CLI, running in Ubuntu 24.04 container with workspace access

### Security Model

**Network isolation:**
- Agent containers cannot access internet directly
- All HTTP/HTTPS traffic routed through proxy (via HTTP_PROXY/HTTPS_PROXY env vars)
- Proxy uses domain whitelist filter with per-agent allowed domains (`agents/<agent>/allowed-domains.txt`)
- Default whitelist (`proxy/allowed-domains.txt`) is empty; each agent defines its own domains
- Merge order: default → agent-specific → user-defined (from `settings.json`)

**File access control:**
- Project directory mounted read-write at the same absolute path as on the host (e.g., `/Users/me/project` on host → `/Users/me/project` in container)
- Agent home directory mirrors host's `$HOME` path (e.g., `/Users/me` on macOS, `/home/me` on Linux), backed by a persistent Docker named volume (`hole-sandbox-agent-home-claude`). Credentials, settings, and CLI state survive sandbox teardown.
- Secret files/folders hidden by mounting /dev/null over them (e.g., .env, .env.local)
- Exclusions configured via `~/.hole/settings.json` (global) and/or `.hole/settings.json` (per-project), merged at runtime

**Agent runs as non-root user:**
- User `agent` created in container (agents/claude/Dockerfile:13)
- Agent CLI installed in user space (~/.local/bin)

## Key Files

- `hole.sh` - CLI tool for managing sandboxes (start command)
- `docker-compose.yml` - Shared infrastructure (proxy service + networks)
- `agents/claude/docker-compose.yml` - Claude agent service definition
- `agents/claude/Dockerfile` - Claude agent image (Ubuntu 24.04 + curl, git, ripgrep, Claude CLI)
- `proxy/Dockerfile` - Proxy image (Alpine + tinyproxy)
- `proxy/tinyproxy.conf` - Proxy configuration (port 8888, filter enabled)
- `proxy/allowed-domains.txt` - Default domain whitelist (empty; shared base for all agents)
- `agents/<agent>/allowed-domains.txt` - Per-agent domain whitelist (regex patterns)

## Development Notes

### Adding Allowed Domains

Agent-specific domains go in `agents/<agent>/allowed-domains.txt` with regex patterns:
```
example\.com
.*\.example\.com
```

The default whitelist (`proxy/allowed-domains.txt`) is empty and serves as the shared base. Per-project user domains are configured via `network.domainWhitelist` in `settings.json`.

### Global Settings

Global defaults can be defined in `~/.hole/settings.json`. This file uses the same schema as per-project `.hole/settings.json`. When both exist, they are deep-merged before use:

- **Objects**: recursively merged; project values win for scalar conflicts
- **Arrays**: concatenated (global first, then project) and deduplicated preserving insertion order

Example `~/.hole/settings.json`:
```json
{
  "files": {
    "exclude": [".env", ".env.local"],
    "include": {
      "~/.npmrc": "~/.npmrc"
    }
  },
  "network": {
    "domainWhitelist": [
      "registry.npmjs.org"
    ]
  }
}
```

If a project also has `.hole/settings.json` with `"files": { "exclude": [".env", "dist"] }`, the merged result will be `[".env", ".env.local", "dist"]`. For `files.include`, unique keys from both are combined; if both define the same key, the project value wins.

### Secret File/Folder Hiding

Per-project exclusions are configured via `.hole/settings.json` in the project directory. The `files.exclude` array lists paths or glob patterns to hide from the agent. The script auto-detects whether each resolved path is a file or directory and generates the correct Docker volume mount:
- **Files** → mounted as `/dev/null:<project_dir>/<path>:ro`
- **Directories** → mounted as anonymous volume at `<project_dir>/<path>`
- **Non-existent paths** → warning printed to stderr, entry skipped

Trailing slashes are stripped automatically (e.g., `node_modules/` → `node_modules`).

**Glob pattern support:** Entries containing `*`, `?`, or `[` are treated as glob patterns and expanded against the project directory at `start` time. Supported syntax:
- `*` — matches any characters within a single path segment (e.g., `.env*` matches `.env`, `.env.local`, `.env.production`)
- `**` — matches zero or more path segments recursively (e.g., `**/secrets` matches `secrets`, `app/secrets`, `app/config/secrets`)
- `?` — matches a single character
- `[abc]` — matches one of the listed characters

Patterns that match no files produce a warning and are skipped. When multiple entries (or overlapping patterns) resolve to the same path, it is mounted only once.

Example `.hole/settings.json`:
```json
{
  "files": {
    "exclude": [
      ".env*",
      "node_modules",
      "apps/*/config",
      "**/secrets"
    ]
  }
}
```

### Environment Variable Expansion

All path settings (`files.include`, `files.exclude`, `libraries`, `hooks.setup.script`) support environment variable expansion. Both `$VAR` and `${VAR}` syntax are supported.

- **Expansion order**: env vars → tilde (`~/`) → relative path resolution
- **Undefined variables**: produce a `log_warn` and are left unexpanded
- **Implementation**: uses bash indirect expansion (`${!var_name}`) — no `eval`

Example `.hole/settings.json`:
```json
{
  "files": {
    "include": {
      "$PROJECT_PATH/shared": "~/shared"
    }
  },
  "libraries": {
    "${SDK_ROOT}/core": "/libs/core"
  }
}
```

### File Inclusion

Additional host files or directories can be mounted into the sandbox via `files.include` in `settings.json` (both global and per-project). This is an object where keys are host paths and values are absolute container paths:

```json
{
  "files": {
    "include": {
      "./shared-config": "~/shared-config",
      "/home/user/data": "/data",
      "~/.npmrc": "~/.npmrc"
    }
  }
}
```

- **Host path resolution:**
  - `$VAR` / `${VAR}` → expanded from environment variables
  - `~/...` → expanded to `$HOME/...`
  - Relative paths → resolved against the project directory
  - Absolute paths → used as-is
- **Container paths** support `~/` (expanded to sandbox home), `/` (absolute), or `$` (env var reference)
- **Non-existent host paths** → warning printed to stderr, entry skipped
- **Trailing slashes** are stripped from both host and container paths
- **Merge behavior**: Since `include` is an object, `deep_merge` handles it correctly — unique keys from both global and project are combined; if both define the same key, project wins

Each entry becomes a bind mount in the agent container: `{resolved_host_path}:{container_path}`.

### Libraries

Additional directories can be mounted **read-only** into the sandbox via `libraries` in `settings.json` (both global and per-project). This is an object where keys are host paths and values are absolute container paths:

```json
{
  "libraries": {
    "~/repos/shared-utils": "/libs/shared-utils",
    "/opt/company/sdk": "/libs/company-sdk",
    "./sibling-project": "/libs/sibling"
  }
}
```

- **Host path resolution**: same as `files.include` (`$VAR` / `${VAR}` → env var, `~/...` → `$HOME/...`, relative → project dir, absolute → as-is)
- **Container paths** support `~/` (expanded to sandbox home), `/` (absolute), or `$` (env var reference)
- **Non-existent or non-directory host paths** → warning printed to stderr, entry skipped
- **Always read-only**: libraries are mounted with `:ro`
- **Merge behavior**: Since `libraries` is an object, `deep_merge` handles it correctly — unique keys from both global and project are combined; if both define the same key, project wins
- **Per-library exclusions**: If a library has its own `.hole/settings.json`, its `files.exclude` entries are resolved against the library source directory and mounted scoped to the library's container mount point. Other settings in the library's `.hole/settings.json` are ignored.

### Project-Specific Domain Whitelist

Per-project domain whitelists are configured via the `network.domainWhitelist` array in `.hole/settings.json`. This allows projects to access additional domains (e.g., npm registry, custom API endpoints) beyond the default allowed domains.

- **Format**: Plain domain names (e.g., `registry.npmjs.org`). Dots are auto-escaped for tinyproxy's regex filter.
- **Merge strategy**: Domains are merged in order: default (`proxy/allowed-domains.txt`) → agent-specific (`agents/<agent>/allowed-domains.txt`) → user-defined. All are included in the final whitelist.
- **Storage**: The merged whitelist file is written to `${TMPDIR:-/tmp}/hole/projects/<project-name>/tinyproxy-domain-whitelist.txt` and bind-mounted into the proxy container.
- **Cleanup**: The whitelist file is removed when the sandbox is destroyed on exit.

Example `.hole/settings.json`:
```json
{
  "files": {
    "exclude": [".env", "node_modules"]
  },
  "network": {
    "domainWhitelist": [
      "registry.npmjs.org",
      "api.github.com"
    ]
  }
}
```

### Dependencies (apt packages)

Additional apt packages can be installed via the `dependencies` array in `settings.json`. This works in both global (`~/.hole/settings.json`) and per-project (`.hole/settings.json`) settings.

- **Format**: apt package names (`python3`) or with version pinning (`python3=3.10.6-1~22.04`)
- **Merge behavior**: Arrays from global and project settings are concatenated and deduplicated (same as other array properties)
- **Installation**: Packages are passed as the `EXTRA_PACKAGES` Docker build arg and installed during image build via a conditional `RUN` layer in the Dockerfile. They are baked into the per-project cached image (`hole-sandbox/agent-claude-${PROJECT_NAME}:latest`), so subsequent sandbox starts are instant.
- **Rebuilding**: Dockerfile is automatically rebuild every start
- **Network**: Since apt runs during `docker build` (with host networking), Ubuntu apt repository domains are **not** added to the sandbox proxy whitelist.

Example `.hole/settings.json`:
```json
{
  "dependencies": [
    "python3",
    "build-essential",
    "htop"
  ]
}
```

Example `~/.hole/settings.json` (global):
```json
{
  "dependencies": [
    "python3",
    "nodejs"
  ]
}
```

If both exist, the merged result includes all unique packages from both files.

### Environment Variables

Custom environment variables can be defined via `environment` in `settings.json` (both global and per-project). This is an object where keys are variable names and values are strings:

```json
{
  "environment": {
    "NODE_ENV": "development",
    "API_URL": "https://api.example.com"
  }
}
```

- **Merge behavior**: Since `environment` is an object, `deep_merge` handles it correctly — unique keys from both global and project are combined; if both define the same key, project wins
- Variables are injected into the agent container's `environment` section in the compose override

### Container Settings

Container options can be configured via `container` in `settings.json` (both global and per-project).

Supported options are

- `container.memoryLimit` → maps to Docker `mem_limit` (e.g., `"8g"`, `"512m"`, `"2048m"`)
- `container.memorySwapLimit` → maps to Docker `memswap_limit` (e.g., `"8g"`, `"512m"`, `"2048m"`)

Example `.hole/settings.json`:
```json
{
  "container": {
    "memoryLimit": "8g",
    "memorySwapLimit": "12g"
  }
}
```

### Setup Hook Script

A custom bash script can be run during the Docker image build via `hooks.setup.script` in `settings.json` (both global and per-project). The script runs as **root** after dependency installation, so it can install system-level packages, configure locales, add apt repositories, etc.

```json
{
  "hooks": {
    "setup": {
      "script": ".hole/my-project-setup.sh"
    }
  }
}
```

- **Path resolution** (same as other settings):
  - `~/...` → expanded to `$HOME/...`
  - Relative paths → resolved against the project directory
  - Absolute paths → used as-is
- **Runs as root** during `docker build`, before switching to the `agent` user
- **Script changes trigger image rebuild** (Docker layer caching based on content)
- **Merge behavior**: Scalar value, so project setting overrides global
- **Non-existent script path** → warning logged to stderr, skipped

### Agent Home Volume

Each agent type has a persistent Docker named volume for its home directory (`hole-sandbox-agent-home-<agent>`). The home directory path mirrors the host's `$HOME` (e.g., `/Users/me` on macOS, `/home/me` on Linux), and the container username matches the host's `$USER`. Lifecycle:

- **Created** by `hole.sh ensure_agent_volume()` on first `start` for that agent
- **Auto-populated** by Docker from the image's home directory contents on first use (CLI binary, `.bashrc`, etc.)
- **Survives** sandbox teardown (`docker compose down --rmi local` does not remove named volumes)
- **Declared `external: true`** in `docker-compose.yml` to prevent accidental removal by `docker compose down -v`
- **Removed** by `uninstall.sh` during full uninstallation

### Docker-in-Docker (DinD) Sidecar

When `container.docker` is `true` in settings, the sandbox includes a `docker:dind` sidecar:

- **Build arg**: `DOCKER_ENABLED` build arg triggers Docker CLI + Compose plugin installation in agent Dockerfiles
- **Compose override**: `generate_instance_compose()` emits the `docker` service definition with proxy env vars, shared project mount (at host absolute path), mirrored file exclusion volumes, and a healthcheck (`docker info`)
- **Agent connection**: `DOCKER_HOST=tcp://docker:2375` (no TLS — internal network only)
- **NO_PROXY**: Agent's `NO_PROXY`/`no_proxy` extended with `docker` to prevent Docker CLI TCP traffic from routing through the HTTP proxy
- **Startup order**: DinD depends on proxy (`service_healthy`), agent depends on DinD (`service_healthy`)
- **Registry access**: Users must whitelist Docker registry domains themselves in `network.domainWhitelist`

### Adding new source files

If you add a new source file to the project, it MUST be added to the `.github/workflows/release.yml` release workflow
to be present in the release.