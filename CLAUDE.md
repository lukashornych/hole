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
- User's Claude config (~/.claude, ~/.claude.json) mounted to persist authentication
- Secret files/folders hidden by mounting /dev/null over them (e.g., .env, .env.local)
- Hardcoded secret folder exclusions (docker-compose.yml:43-47) - needs to be made extensible per project

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

**Destroy a sandbox:**
```bash
./hole.sh claude destroy /path/to/project
```

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
4. **Agent container removed on exit** - proxy remains running until `destroy` command

### Key Behavior

- **Clean sandbox creation**: Each `start` creates a fresh agent container
- **Agent auto-cleanup**: Agent container is automatically removed on CLI exit
- **Persistent proxy**: Proxy container continues running until explicit `destroy`
- **Unique naming**: Project names based on absolute path prevent collisions between projects
- **Explicit cleanup**: Use `destroy` command to tear down proxy and remove all resources

## Key Files

- `hole.sh` - Unified CLI tool for managing sandboxes (start, destroy commands)
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

### Container Naming

Project name derived from sanitized absolute path: `hole-$(sanitized_absolute_path)`

Example: `/Users/lho/www/oss/hole` → `hole-users-lho-www-oss-hole`

This ensures:
- Multiple projects can have sandboxes simultaneously
- Same project directory always gets same container name (deterministic based on absolute path)
- Clean separation between different sandboxes
- No collisions between projects with same directory name in different locations
