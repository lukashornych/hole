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
hole start claude . --rebuild             # force rebuild of cached Docker images
```

`--debug` sets up the sandbox normally but drops you into an interactive shell for inspecting volumes, network connectivity, and installed packages.

`--dump-network-access` writes a `claude-network-access-{id}.log` file to the project directory after the agent exits, containing a sorted list of distinct domains (both allowed and denied).

`--rebuild` forces a fresh build of the sandbox Docker images. Sandbox images are cached per-project for fast startup — use this flag after changing `dependencies`, hook scripts, or when the base agent image needs updating.

### Other commands

```sh
hole destroy .                # remove cached Docker images for this project
hole destroy /path/to/project # remove cached Docker images for a specific project
hole help                     # show usage information
hole version                  # print installed version
hole update                   # update to the latest release
```

`hole destroy` removes all project-related Docker resources including cached agent and proxy images for all agent types.
It also cleans up any lingering containers, networks, and temp files that may remain after a crash. 
The shared agent home volume (credentials) is preserved — it is only removed by `uninstall.sh`.

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

```sh
hole update
```

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

Use plain domain names — dots are auto-escaped for the proxy filter. After changing the whitelist, restart the sandbox (changes take effect on next `hole start`).

### Dependencies

Install additional apt packages at container startup:

```json
{
  "dependencies": ["python3", "build-essential", "htop"]
}
```

Packages are installed during the Docker image build and baked into the cached per-project image, so subsequent startups are instant. After changing the dependency list, use `--rebuild` to apply the changes.

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

### Hooks

Hooks allow you to inject some logic into sandbox lifecycle.

#### Setup hook

Run a custom bash script during the Docker image build to perform system-level setup (install packages, configure locales, add apt repositories, etc.):

```json
{
  "hooks": {
    "setup": {
      "script": ".hole/setup.sh"
    }
  }
}
```

The script runs as **root** during the image build, after dependency installation. Host paths starting with `~/` expand 
to `$HOME`, relative paths resolve against the project directory, and absolute paths are used as-is. 
Non-existent paths are skipped with a warning.

**Important:** The agent home directory (`/home/agent`) is backed by a persistent Docker volume that overrides image contents.
Do not install anything to `/home/agent` in the setup script — it will be hidden by the volume mount.

Use `--rebuild` to force a fresh build if needed.
