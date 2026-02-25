# Hole

## What is Hole?

Hole is a CLI tool for running AI agents in isolated Docker sandboxes.

Running AI agents directly on your host machine is risky — they have access to your filesystem, network, and credentials. Built-in agent sandboxes (e.g. Claude Code's) can potentially be bypassed by the agent itself, since the agent controls the process.

Hole provides true isolation through:

- **Network control** — all traffic routes through a proxy with a domain whitelist; the agent cannot reach the internet directly
- **File access control** — project files are mounted into the container, with configurable exclusions (e.g. `.env`, `node_modules`) hidden from the agent
- **Containerized execution** — the agent runs as a non-root user inside a Docker container that is destroyed on exit

## Usage

Start a sandbox for a supported agent (`claude`) in a project directory:

```sh
hole start claude .
# or
hole start claude /path/to/project
```

The sandbox is created from scratch each time and fully destroyed when you exit the agent CLI. Multiple sandboxes can run simultaneously for the same project.

Credentials persist across sessions in a Docker volume. On first run, the volume is created automatically — log in inside the sandbox using `/login` in Claude Code. You only need to log in once.

### Flags

```sh
hole start claude . --debug               # open a bash shell instead of the agent CLI
hole start claude . --dump-network-access  # write accessed domains to a log file on exit
```

`--debug` sets up the sandbox normally but drops you into an interactive shell for inspecting volumes, network connectivity, and installed packages.

`--dump-network-access` writes a `claude-network-access-{id}.log` file to the project directory after the agent exits, containing a sorted list of distinct domains (both allowed and denied).

### Other commands

```sh
hole help      # show usage information
hole version   # print installed version
```

## Installation

**Supported platforms:** Linux, macOS, WSL

**Requirements:** `curl` or `wget`, `tar`, `docker`, `jq`, `jv`

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
# or
wget -qO- https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
```

If `~/.local/bin` is not in your `PATH`, add it to your shell profile (`.bashrc` / `.zshrc`):

```sh
export PATH="$HOME/.local/bin:$PATH"
```

### Update

Run the same install command again — the installer detects an existing installation, removes it, and reinstalls from the latest release.

Exit any running sandboxes before updating.

### Uninstall

Exit any running sandboxes first, then run:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
# or
wget -qO- https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
```

## Configuration

Settings are defined in `~/.hole/settings.json` (global) and/or `.hole/settings.json` (per-project). When both exist, they are deep-merged: objects are recursively merged (project values win for scalar conflicts), arrays are concatenated and deduplicated (global items first).

### File exclusions

Hide files and directories from the agent:

```json
{
  "files": {
    "exclude": [".env", ".env.local", "node_modules", "dist"]
  }
}
```

Files are mounted as `/dev/null` and directories as empty anonymous volumes inside the container. Non-existent paths are skipped with a warning.

### File inclusions

Mount additional host files or directories into the sandbox. Keys are host paths, values are absolute container paths:

```json
{
  "files": {
    "include": {
      "~/.npmrc": "/home/agent/.npmrc",
      "./shared-config": "/workspace/shared-config",
      "/home/user/data": "/data"
    }
  }
}
```

Host paths starting with `~/` expand to `$HOME`, relative paths resolve against the project directory, and absolute paths are used as-is. Non-existent paths are skipped with a warning.

### Domain whitelist

By default, agents can only reach domains required for their operation (e.g. `api.anthropic.com` for Claude). Allow additional domains:

```json
{
  "network": {
    "domainWhitelist": ["registry.npmjs.org", "api.github.com"]
  }
}
```

Use plain domain names — dots are auto-escaped for the proxy filter.

### Dependencies

Install additional apt packages at container startup:

```json
{
  "dependencies": ["python3", "build-essential", "htop"]
}
```

Packages are installed before the agent CLI starts. Ubuntu apt repository domains are automatically added to the proxy whitelist when dependencies are specified.

### Container settings

Configure container resource limits:

```json
{
  "container": {
    "memoryLimit": "8g",
    "memorySwapLimit": "12g"
  }
}
```

- `memoryLimit` — Docker `mem_limit` (e.g. `"8g"`, `"512m"`)
- `memorySwapLimit` — Docker `memswap_limit` (e.g. `"8g"`, `"512m"`)
