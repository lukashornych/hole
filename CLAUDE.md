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
./sandbox.sh claude /path/to/project
```
Or from within a project directory:
```bash
./sandbox.sh claude .
```

**Destroy a sandbox:**
```bash
./sandbox-destroy.sh /path/to/project
```

### How It Works

1. `sandbox.sh` derives unique project name from directory basename (e.g., "sandbox-myproject")
2. Starts proxy container in background with health check
3. Runs Claude container interactively with:
   - PROJECT_DIR env var set to target directory
   - COMPOSE_PROJECT_NAME for unique container naming
   - Proxy dependency (waits for healthy status)
4. Cleanup trap on EXIT tears down all containers via `docker compose down`

## Key Files

- `sandbox.sh` - Main launcher script (starts proxy + agent)
- `sandbox-destroy.sh` - Cleanup script (tears down containers + removes images)
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
5. Update `sandbox.sh` to validate agent parameter

### Secret File/Folder Hiding

Currently hardcoded in docker-compose.yml:40-47. To extend:
- Add volume mounts pointing to /dev/null (for files)
- Add empty volume mounts (for directories)

TODO: Make this extensible via configuration file per project.

### Container Naming

Project name derived as: `sandbox-$(basename "$TARGET_DIR")`

This ensures:
- Multiple projects can have sandboxes simultaneously
- Same project directory always gets same container name
- Clean separation between different sandboxes
