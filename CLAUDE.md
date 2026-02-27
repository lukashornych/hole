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
- use shell strict mode
- always Double-Quote Variables
- prefer ${VAR} Syntax
- use Lowercase for Local Variables
- use $() for Command Substitution
- use [[ ]] for Conditionals
- use Arithmetic Expansion (( )) for math
- use `getopts` for command-line argument parsing
- log using sourced logger.sh library (log_info, log_error, log_warn, log_line), do not use echo for logging

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
- Proxy uses domain whitelist filter (proxy/allowed-domains.txt) to restrict access
- Currently allowed domains for Claude: api.anthropic.com, claude.ai, platform.claude.com

**File access control:**
- Project directory mounted read-write at /workspace
- Agent home directory (`/home/agent`) backed by a persistent Docker named volume (`hole-agent-home-claude`). Credentials, settings, and CLI state survive sandbox teardown.
- Secret files/folders hidden by mounting /dev/null over them (e.g., .env, .env.local)
- Exclusions configured via `~/.hole/settings.json` (global) and/or `.hole/settings.json` (per-project), merged at runtime

**Agent runs as non-root user:**
- User `agent` created in container (agents/claude/Dockerfile:13)
- Agent CLI installed in user space (~/.local/bin)

## Key Files

- `hole.sh` - CLI tool for managing sandboxes (start command)
- `docker-compose.yml` - Service orchestration with profiles (claude, gemini)
- `agents/claude/Dockerfile` - Claude agent image (Ubuntu 24.04 + curl, git, ripgrep, Claude CLI)
- `proxy/Dockerfile` - Proxy image (Alpine + tinyproxy)
- `proxy/tinyproxy.conf` - Proxy configuration (port 8888, filter enabled)
- `proxy/allowed-domains.txt` - Domain whitelist (regex patterns)

## Development Notes

### Adding Allowed Domains

Edit `proxy/allowed-domains.txt` with regex patterns:
```
example\.com
.*\.example\.com
```

Rebuild proxy for changes to take effect:
```bash
docker compose -p <project-name> up -d --build proxy
```

### Adding New Agent Types

1. Create `agents/<agent-name>/Dockerfile`
2. Add service definition in `docker-compose.yml` under a new profile
3. Configure proxy dependency and network: sandbox only
4. Add allowed domains to `proxy/allowed-domains.txt`
5. Update `hole.sh` VALID_AGENTS array to include the new agent
6. Update `uninstall.sh` agents array to include the new agent (for volume cleanup)

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
      "~/.npmrc": "/home/agent/.npmrc"
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

Per-project exclusions are configured via `.hole/settings.json` in the project directory. The `files.exclude` array lists paths to hide from the agent. The script auto-detects whether each entry is a file or directory and generates the correct Docker volume mount:
- **Files** → mounted as `/dev/null:/workspace/<path>:ro`
- **Directories** → mounted as anonymous volume at `/workspace/<path>`
- **Non-existent paths** → warning printed to stderr, entry skipped

Trailing slashes are stripped automatically (e.g., `node_modules/` → `node_modules`).

Example `.hole/settings.json`:
```json
{
  "files": {
    "exclude": [
      ".env",
      ".env.local",
      "node_modules",
      "dist"
    ]
  }
}
```

### File Inclusion

Additional host files or directories can be mounted into the sandbox via `files.include` in `settings.json` (both global and per-project). This is an object where keys are host paths and values are absolute container paths:

```json
{
  "files": {
    "include": {
      "./shared-config": "/workspace/shared-config",
      "/home/user/data": "/data",
      "~/.npmrc": "/home/agent/.npmrc"
    }
  }
}
```

- **Host path resolution:**
  - `~/...` → expanded to `$HOME/...`
  - Relative paths → resolved against the project directory
  - Absolute paths → used as-is
- **Container paths** must be absolute (enforced by schema `^/` pattern)
- **Non-existent host paths** → warning printed to stderr, entry skipped
- **Trailing slashes** are stripped from both host and container paths
- **Merge behavior**: Since `include` is an object, `deep_merge` handles it correctly — unique keys from both global and project are combined; if both define the same key, project wins

Each entry becomes a bind mount in the agent container: `{resolved_host_path}:{container_path}`.

### Project-Specific Domain Whitelist

Per-project domain whitelists are configured via the `network.domainWhitelist` array in `.hole/settings.json`. This allows projects to access additional domains (e.g., npm registry, custom API endpoints) beyond the default allowed domains.

- **Format**: Plain domain names (e.g., `registry.npmjs.org`). Dots are auto-escaped for tinyproxy's regex filter.
- **Merge strategy**: Default domains from `proxy/allowed-domains.txt` are always included. Project-specific domains are appended.
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

Additional apt packages can be installed at container startup via the `dependencies` array in `settings.json`. This works in both global (`~/.hole/settings.json`) and per-project (`.hole/settings.json`) settings.

- **Format**: apt package names (`python3`) or with version pinning (`python3=3.10.6-1~22.04`)
- **Merge behavior**: Arrays from global and project settings are concatenated and deduplicated (same as other array properties)
- **Network**: When dependencies are specified, Ubuntu apt repository domains (`archive.ubuntu.com`, `security.ubuntu.com`) are automatically added to the proxy whitelist
- **Installation**: Packages are installed via `sudo apt-get install` in `entrypoint.sh` at container startup, before the agent CLI starts

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

### Container Naming

Project name derived from sanitized absolute path plus a random instance ID: `hole-$(sanitized_absolute_path)-$(agent)-$(instance_id)`

Example: `/Users/lho/www/oss/hole` with claude agent → `hole-users-lho-www-oss-hole-claude-a1b2c3`

This ensures:
- Multiple sandboxes can run simultaneously for the same project
- Clean separation between different sandboxes
- No collisions between projects with same directory name in different locations

### Version Check & Update

`hole.sh` includes a version check mechanism and an `update` command:

- **Silent version check**: Runs during `start` and `version` commands with a 1-second timeout. Compares the installed version (`version` file) against the latest GitHub release. Prints a one-line notice if a newer version exists. Skipped in dev mode (no version file). Network failures are silently ignored.
- **`hole update` command**: Fetches the latest version from the GitHub API. If newer, downloads and runs `install.sh` from the main branch. Errors on dev installations (no version file).
- **GitHub constants**: `GITHUB_REPO`, `GITHUB_API`, `GITHUB_INSTALL_SCRIPT` defined at the top of `hole.sh`.

### Agent Home Volume

Each agent type has a persistent Docker named volume for its home directory (`hole-agent-home-<agent>`). Lifecycle:

- **Created** by `hole.sh ensure_agent_volume()` on first `start` for that agent
- **Auto-populated** by Docker from the image's `/home/agent` contents on first use (CLI binary, `.bashrc`, etc.)
- **Survives** sandbox teardown (`docker compose down --rmi local` does not remove named volumes)
- **Declared `external: true`** in `docker-compose.yml` to prevent accidental removal by `docker compose down -v`
- **Removed** by `uninstall.sh` during full uninstallation
