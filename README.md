# Hole

## What is Hole?

Hole is a CLI tool for running AI agents in isolated Docker sandboxes.

Running AI agents directly on your host machine is risky — they have access to your filesystem, network, and credentials. Built-in agent sandboxes (e.g. Claude Code's) can potentially be bypassed by the agent itself, since the agent controls the process.

Hole provides true isolation through:

- **Network control** — all traffic routes through a proxy with a domain whitelist; the agent cannot reach the internet directly
- **File access control** — project files are mounted into the container, with configurable exclusions (e.g. `.env`, `node_modules`) hidden from the agent
- **Containerized execution** — the agent runs as a non-root user inside a Docker container that is destroyed on exit

Table of contents:

- [What is Hole?](#what-is-hole)
- [Usage](#usage)
  - [Flags](#flags)
  - [Passing arguments to the agent](#passing-arguments-to-the-agent)
  - [Other commands](#other-commands)
- [Installation](#installation)
  - [Update](#update)
  - [Uninstall](#uninstall)
- [Agents](#agents)
  - [Claude Code](#claude-code)
  - [Gemini CLI](#gemini-cli)
  - [Codex CLI](#codex-cli)
- [Configuration](#configuration)
  - [Project .gitignore](#project-gitignore)
  - [File exclusions](#file-exclusions)
  - [Environment variable expansion](#environment-variable-expansion)
  - [File inclusions](#file-inclusions)
  - [Libraries](#libraries)
  - [Domain whitelist](#domain-whitelist)
  - [Dependencies](#dependencies)
  - [Container settings](#container-settings)
  - [Docker-in-Docker](#docker-in-docker)
  - [Hooks](#hooks)
  - [Configuration examples](#configuration-examples)

## Usage

Start a sandbox for a supported agent in a project directory:

```shell
hole start {agent} {project path}
```

for example:

```sh
hole start claude .
# or
hole start claude /path/to/project
```

The sandbox is created from scratch each time and fully destroyed when you exit the agent CLI. Multiple sandboxes can run simultaneously for the same project.

The entire home directory is mounted as a persistent Docker volume (`hole-sandbox-agent-home`), shared across all agent types.
This allows for credentials to persist across sandbox instances. On first run, the volume is created automatically.

All enabled agents are installed into a single unified sandbox image. By default, all supported agents (claude, gemini, codex) are installed, so any agent can invoke other agents from within the sandbox. The `agent` parameter only determines the startup command.

### Flags

```sh
hole start {agent} {project path} --debug
hole start {agent} {project path} --dump-network-access
hole start {agent} {project path} --rebuild
hole start {agent} {project path} --unrestricted-network
hole start {agent} {project path} --with-docker
```

`-d`, `--debug` sets up the sandbox normally but drops you into an interactive shell for inspecting volumes, network connectivity, and installed packages.

`-n`, `--dump-network-access` writes a `.hole/logs/network-access-{agent}-{instance id}.log` file to the project directory after the agent exits, containing a sorted list of distinct domains (both allowed and denied).

`-r`, `--rebuild` forces a fresh build of the sandbox Docker images. Sandbox images are cached per-project for fast startup — use this flag after changing `dependencies`, hook scripts, or when the base agent image needs updating.

`-u`, `--unrestricted-network` disables domain whitelist filtering, allowing the agent to access any domain. Traffic still flows through the proxy, so `--dump-network-access` logging continues to work. This is useful when the agent needs broad internet access and maintaining a whitelist is impractical.

`--with-docker` enables a Docker-in-Docker sidecar for the sandbox, allowing the agent to run `docker` and `docker compose` commands. This is equivalent to setting `container.docker: true` in settings. See [Docker-in-Docker](#docker-in-docker) for details.

### Passing arguments to the agent

Use `--` to separate hole flags from agent-specific arguments. Everything after `--` is passed directly to the agent CLI:

```sh
hole start claude . -- -p "explain this function"
hole start claude . --rebuild -- --output-format stream-json
hole start gemini . -- -p "refactor this code"
```

The base command for each agent (e.g. `claude --dangerously-skip-permissions`) is defined in `agents/<agent>/command.json`. User arguments are appended to this base command.

Note: `--debug` and agent arguments cannot be used together.

### Other commands

```sh
hole destroy {project path}   # remove all project-related Docker resources including cached agent and proxy images
hole help                     # show usage information
hole version                  # print installed version
hole update                   # update to the latest release
hole uninstall                # uninstall hole and optionally remove Docker resources
```

## Installation

**Supported platforms:** Linux, macOS, and WSL

**Requirements:** `curl` or `wget`, `tar`, [`docker`](https://www.docker.com/get-started/) or [`podman`](https://podman.io/docs/installation) (with compose plugin), [`jq`](https://jqlang.github.io/jq/download/), [`jv`](https://github.com/santhosh-tekuri/jsonschema/releases)

> _Note: `jv` utility documentation mentions installation through golang, you don't have to do that, you can download the binary from their [release page](https://github.com/santhosh-tekuri/jsonschema/releases) and place it in your bin folder you have in your PATH._

**Optional:** `flock` (from `util-linux`) — enables persistent Docker image caching across sandbox restarts when using Docker-in-Docker. Pre-installed on most Linux distributions; on macOS, install via `brew install util-linux`.

**Container runtime:** Hole auto-detects Docker or Podman (Docker is preferred when both are available). To override the auto-detection, set the `HOLE_RUNTIME` environment variable:

```sh
export HOLE_RUNTIME=podman
```

> _Note: When using Podman, ensure `podman compose` is available (via `podman-compose` or the Podman Compose plugin). Rootless Podman may require additional configuration for bind mount permissions._

To install the latest version, run the following command in your terminal:

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

The update command will update to the newest version available. During the update, all existing sandboxes and their resources
(Docker images, networks, volumes besides agent home volumes) will be stopped and removed to avoid any incompatiblity with the new version.

### Uninstall

```sh
hole uninstall
```

The uninstall command will remove all application data as well as sandbox resources (Docker images, networks, volumes, etc.).

## Agents

These are currently supported agents: [Claude Code](https://claude.com/product/claude-code), [Gemini CLI](https://github.com/google-gemini/gemini-cli), [Codex CLI](https://github.com/openai/codex)

### Claude Code

Start a sandbox with Claude Code agent:

```shell
hole start claude .
# or
hole start claude /path/to/project
```

#### Example configurations

##### Passing custom status line script

Add following includes to `~/.hole/settings.json`:

```json
{
    "files": {
        "include": {
            "~/.claude/statusline-command.sh": "~/.claude/statusline-command.sh",
            "~/.claude/settings.json": "~/.claude/settings.json"
        }
    }
}
```

This can of course change slightly based on how your status line script is named. Also, make sure the script is executable on your host system.

##### Passing global skills

Add following includes to `~/.hole/settings.json`:

```json
{
    "files": {
        "include": {
            "~/.claude/skills": "~/.claude/skills"
        }
    }
}
```

This will make sure you don't lose your global skils if you uninstall Hole.

##### Passing Claude settings.json

Add following includes to `~/.hole/settings.json`:

```json
{
    "files": {
        "include": {
            "~/.claude/settings.json": "~/.claude/settings.json"
        }
    }
}
```

##### Adding marketplaces

Marketplaces are usually added via SSH repositories, but, you usually don't want to give your SSH keys to the agent.
Fortunately, you can add marketplaces via HTTPS repositories, for example:

```shell
/plugin marketplace add https://github.com/anthropics/claude-plugins-official.git
```

You will also need to whitelist the marketplace domain in the agent [settings](#domain-whitelist).

If your marketplace is private, you will need also need some sort of authentication, usually via a personal access token:

```shell
/plugin marketplace add https://{username}:{pat}@gitlab.mydomain.com/internal/claude-marketplace.git
```

### Gemini CLI

Start a sandbox with Gemini CLI agent:

```shell
hole start gemini .
# or
hole start gemini /path/to/project
```

> **Note:** there is an issue with initial login where it freezes the agent after a successful login. To work around this,
> start the agent normally and login, then in another terminal in same project run `hole destroy {project path}`. Then
> you can start the agent again, and you should be logged in.

### Codex CLI

Start a sandbox with Codex CLI agent:

```shell
hole start codex .
# or
hole start codex /path/to/project
```

## Configuration

Settings are defined in `~/.hole/settings.json` (global) and/or `.hole/settings.json` (per-project). When both exist, they are deep-merged: objects are recursively merged (project values win for scalar conflicts), arrays are concatenated and deduplicated (global items first).

You can find JSON chema definition at `~/.local/share/hole/schema/settings.schema.json`.

### Project .gitignore

It is recommended to add the following paths to your project `.gitignore` to avoid accidentally committing unwanted files:

```
.hole/logs/
```

### File exclusions

> _Note: if the excluded files are already in Git history, the agent may potentically find it in the history if you don't exclude entire `.git` folder._

Hide files and directories from the agent:

```json
{
  "files": {
    "exclude": [".env", ".env.local", "node_modules", "dist"]
  }
}
```

Files are mounted as `/dev/null` and directories as empty anonymous volumes inside the container. Non-existent paths are skipped with a warning.

Paths support environment variable expansion (`$VAR`, `${VAR}`) and glob patterns are supported for matching multiple paths at once:

```json
{
  "files": {
    "exclude": [".env*", "apps/*/config", "**/secrets"]
  }
}
```

- `*` — matches any characters within a single path segment (e.g. `.env*` matches `.env`, `.env.local`, `.env.production`)
- `**` — matches zero or more path segments recursively (e.g. `**/secrets` matches `secrets`, `app/secrets`, `app/config/secrets`)
- `?` — matches a single character
- `[abc]` — matches one of the listed characters

Patterns that match no files produce a warning and are skipped. Duplicate paths (from overlapping patterns) are mounted only once.
Undefined variables produce a warning and are left unexpanded.

### File inclusions

Mount additional host files or directories into the sandbox. Keys are host paths, values are container paths:

```json
{
  "files": {
    "include": {
      "~/.npmrc": "~/.npmrc",
      "./shared-config": "~/shared-config",
      "/home/user/data": "/data"
    }
  }
}
```

Both host and container paths support environment variable expansion (`$VAR`, `${VAR}`) and tilde expansion (`~/`). Host paths also support relative paths (resolved against the project directory). Container `~/` expands to the sandbox home directory (which mirrors the host's `$HOME`). 

Non-existent paths are skipped with a warning. Undefined variables produce a warning and are left unexpanded.

### Libraries

Mount additional directories read-only into the sandbox. This is useful for giving the agent access to shared libraries, SDKs, or sibling projects as reference material. Keys are host paths, values are absolute container paths:

```json
{
  "libraries": {
    "~/repos/shared-utils": "/libs/shared-utils",
    "/opt/company/sdk": "/libs/company-sdk",
    "./sibling-project": "/libs/sibling"
  }
}
```

Both host and container paths support environment variable expansion (`$VAR`, `${VAR}`) and tilde
expansion (`~/`). Host paths also support relative paths (resolved
against the project directory).

Non-existent paths are skipped with a warning. Undefined variables produce a warning and are left unexpanded.

Libraries are always mounted **read-only**.

If a library has its own `.hole/settings.json`, its `files.exclude` entries are applied scoped to that library's mount point (not mixed with the main project's exclusions). Other settings in the library's `.hole/settings.json` are ignored.

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

- `baseImage` — custom base Docker image for the agent container (defaults to `ubuntu:24.04`). The image must be based on Ubuntu 24.04; other base images may work but are not tested.
- `memoryLimit` — Docker `mem_limit` (e.g. `"8g"`, `"512m"`)
- `memorySwapLimit` — Docker `memswap_limit` (e.g. `"8g"`, `"512m"`)
- `enabledAgents` — array of agent names to install in the sandbox (defaults to all: `["claude", "gemini", "codex"]`)

To install only specific agents (e.g., to reduce image size):

```json
{
  "container": {
    "enabledAgents": ["claude", "gemini"]
  }
}
```

The startup agent must be in the enabled list, otherwise `hole start` will fail with an error.

### Docker-in-Docker

Enable an isolated Docker daemon inside the sandbox so the agent can run `docker` and `docker compose` (e.g., to spin up PostgreSQL, Redis, or other services for tests).

Via settings:

```json
{
  "container": {
    "docker": true
  }
}
```

Or ad-hoc via startup flag:

```sh
hole start claude . --with-docker
```

When enabled, a `docker:dind` sidecar container starts on the internal `sandbox` network. The agent gets Docker CLI and Compose plugin installed automatically.

**Registry domain whitelist:** Image pulls go through the sandbox proxy, so you must whitelist your registry domains. For Docker Hub, add:

```json
{
  "network": {
    "domainWhitelist": [
      "registry-1.docker.io",
      "auth.docker.io",
      "production.cloudflare.docker.com",
      "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
      "docker-images-prod.r2.cloudflarestorage.com"
    ]
  }
}
```

For other registries (GitHub Container Registry, AWS ECR, etc.), add the corresponding domains.

**Accessing services:** Containers started inside DinD are reachable from the agent at hostname `docker`, not `localhost`. For example, if you run PostgreSQL on port 5432 inside DinD, connect to `docker:5432` from the agent. When exposing ports in `docker run` or `docker-compose.yml`, bind to all interfaces (e.g., `3307:3306`) rather than localhost (e.g., `127.0.0.1:3307:3306`), because the agent connects to the DinD sidecar over the Docker network, not via loopback.

**Workspace bind mounts:** The project directory is mounted at the same absolute path as on the host in both the agent and DinD containers, so bind mounts in user `docker-compose.yml` files resolve correctly.

**File exclusions:** Exclusion volumes from the agent are mirrored on the DinD container's project mount, so `docker compose` files cannot access excluded secrets.

**Persistent image cache:** Each DinD sidecar gets its own ephemeral instance volume (`hole-sandbox-docker-data-<instance>`), seeded on start from a global cache volume (`hole-sandbox-docker-cache`). On teardown the instance data is synced back to the cache and the instance volume is removed. This means images survive sandbox teardown (via the cache) and do not need to be re-downloaded, while multiple sandboxes (even across different projects) can run simultaneously without conflicts (each has its own `/var/lib/docker`). Images pulled in one project are available to seed any other project. The cache volume is preserved during `hole update` (soft-wipe) and only removed on full `hole uninstall`.

**Security:** The DinD container runs with `privileged: true`, which is required for Docker-in-Docker. This is contained within the isolated sandbox network — the DinD container has no direct internet access (all traffic routes through the proxy).

Use `--rebuild` after enabling this setting for the first time to install the Docker CLI in the agent image.

### Environment variables

Define custom environment variables for the agent container:

```json
{
  "environment": {
    "NODE_ENV": "development",
    "API_URL": "https://api.example.com"
  }
}
```

Variables are set in the agent container at startup. When Docker-in-Docker is enabled, these variables are also passed to the DinD sidecar container. Since `environment` is an object, global and project settings are deep-merged — unique keys from both are combined, and if both define the same key, the project value wins.

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

The script runs as the agent user during the image build, after dependency installation. Host paths support environment variable expansion (`$VAR`, `${VAR}`), tilde expansion (`~/`), relative paths (resolved against the project directory), and absolute paths. Non-existent paths are skipped with a warning.

**Important:** The agent home directory (mirrors host's `$HOME`, e.g., `/Users/me` on macOS) is backed by a persistent Docker volume that overrides image contents.
Do not install anything to the agent home directory in the setup script — it will be hidden by the volume mount.

Use `--rebuild` to force a fresh build if needed.

### Configuration examples

#### Run Maven compilation and tests

To use Maven inside sandboxes (e.g.: to run test by the agent), you need some special configuration for the sandbox.

_Note: the following configurations can be added to global `~/.hole/settings.json` file, or project-specific `.hole/settings.json` file._

##### 1. Create Maven settings for agent

We don't want to pass the main `~/.m2/settings.xml` with potential secrets into the sandbox. Also, we need some special
proxy configuration for the agent. Therefore, it is advisable to create separate `~/.m2/agent-settings.xml` file.

The main configuration needed by the agent is as follows:

```xml
<proxies>
    <proxy>
        <id>http-internet</id>
        <active>true</active>
        <protocol>http</protocol>
        <host>proxy</host>
        <port>8888</port>
    </proxy>
    <proxy>
        <id>https-internet</id>
        <active>true</active>
        <protocol>https</protocol>
        <host>proxy</host>
        <port>8888</port>
    </proxy>
</proxies>
```

the rest is up to you.

You can also set up specific toolchains settings for the agent, if you need. The tricky part is specifying the JDK installation folder
inside the agent. If JDK is installed through `dependencies`, it should be found at:

```
/usr/lib/jvm/java-17-openjdk-amd64  // for x86 devices (e.g. standard Linux machine, WSL)
/usr/lib/jvm/java-17-openjdk-arm64  // for ARM devices (e.g. macOS)
```

With that information, create file `~/.m2/agent-toolchains.xml` and set it up:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<toolchains>
    <toolchain>
    <type>jdk</type>
    <provides>
        <version>17</version>
        <vendor>openjdk</vendor>
    </provides>
    <configuration>
        <jdkHome>/usr/lib/jvm/java-17-openjdk-arm64</jdkHome>
    </configuration>
    </toolchain>
</toolchains>
```

##### 2. Include Maven settings into agent

```json
{
  "files": {
    "include": {
      "~/.m2/repository": "~/.m2/repository",
      "~/.m2/agent-settings.xml": "~/.m2/settings.xml",
      "~/.m2/agent-toolchains.xml": "~/.m2/toolchains.xml" // optional, only if you use toolchains
    }
  }
}
```

##### 3. Allow internal Maven repositories

The sandbox denies all network traffic from not-whitelisted domains. To run Maven, you typically need at least `apache.org`
domain for standard repositories. If you have some internal repositories, include them too:

```json
{
  "network": {
    "domainWhitelist": [
      "apache.org"
    ]
  }
}
```
