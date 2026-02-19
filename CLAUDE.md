# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hole is a CLI tool for creating and managing sandboxes for AI agents. It provides:
- Network access control via proxy with domain whitelist
- File access control via Docker volume mounts
- Isolated execution environment using Docker containers

Currently supports Claude Code agent (with placeholder for Gemini agent).

## Architecture

The project uses Docker Compose to orchestrate a multi-container sandbox environment:

### Container Architecture

**Two-network design for security:**
- `sandbox` network: Internal network (no direct internet access) where agents run
- `internet` network: Bridge network that only the proxy can access

**Two main services:**
- `proxy`: Tinyproxy-based HTTP/HTTPS proxy that filters requests to allowed domains only (proxy/allowed-domains.txt)
- `claude`: Claude Code CLI agent running in Ubuntu 22.04 container with workspace access

### Security Model

**Network isolation:**
- Agent containers cannot access internet directly
- All HTTP/HTTPS traffic routed through proxy (via HTTP_PROXY/HTTPS_PROXY env vars)
- Proxy uses domain whitelist filter (proxy/allowed-domains.txt) to restrict access
- Currently allowed domains for Claude: api.anthropic.com, claude.ai, platform.claude.com

**File access control:**
- Project directory mounted read-write at /workspace
- `~/.claude` directory: mounted directly at `/home/claude/.claude` in read-write mode. Changes (plans, project settings, etc.) persist back to the host.
- `~/.claude.json` file: mounted read-only to staging dir (`/home/claude/.host-config/`), then copied to the home dir at container startup by `entrypoint.sh`. Avoids atomic-write corruption. Authentication is handled by `CLAUDE_CODE_OAUTH_TOKEN` env var, so config writes don't need to persist back to the host.
- Secret files/folders hidden by mounting /dev/null over them (e.g., .env, .env.local)
- Per-project exclusions configured via `.hole/settings.json`

**Agent runs as non-root user:**
- User `claude` created in container (agents/claude/Dockerfile:11)
- Claude Code CLI installed in user space (~/.local/bin)

## Usage

### Setup (first time)

Before using the Claude sandbox, set up authentication:
1. Install Claude Code locally
2. Run `claude setup-token` and login
3. Store the OAuth token in your shell profile (.bashrc/.zshrc):
   ```bash
   export CLAUDE_CODE_OAUTH_TOKEN="sk-ant-..."
   ```

### Running the Sandbox

**Start a sandbox:**
```bash
./hole.sh claude start /path/to/project
```
Or from within a project directory:
```bash
./hole.sh claude start .
```

The sandbox is fully destroyed when you exit the agent CLI.

**Get help:**
```bash
./hole.sh help
```

### How It Works

1. `hole.sh` derives unique project name from sanitized absolute path (e.g., "hole-users-lho-www-oss-myproject")
2. Starts proxy container in detached mode with health check
3. Runs agent container interactively using `docker compose run`:
   - PROJECT_DIR env var set to target directory
   - COMPOSE_PROJECT_NAME for unique container naming based on absolute path
   - Proxy dependency (waits for healthy status)
   - Allocates TTY and connects stdin for interactive CLI
4. **Full teardown on exit** - all containers, networks, and per-project config are removed when the agent CLI exits

### Key Behavior

- **Fresh sandbox each time**: Each `start` creates a new sandbox from scratch
- **Auto-destroy on exit**: When you exit the agent CLI, the entire sandbox (containers, networks, images, per-project config) is automatically destroyed
- **Unique naming**: Project names based on absolute path prevent collisions between projects

## Key Files

- `hole.sh` - CLI tool for managing sandboxes (start command)
- `docker-compose.yml` - Service orchestration with profiles (claude, gemini)
- `agents/claude/Dockerfile` - Claude agent image (Ubuntu 22.04 + curl, git, ripgrep, Claude CLI)
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

### Container Naming

Project name derived from sanitized absolute path plus a random instance ID: `hole-$(sanitized_absolute_path)-$(agent)-$(instance_id)`

Example: `/Users/lho/www/oss/hole` with claude agent → `hole-users-lho-www-oss-hole-claude-a1b2c3`

This ensures:
- Multiple sandboxes can run simultaneously for the same project
- Clean separation between different sandboxes
- No collisions between projects with same directory name in different locations
