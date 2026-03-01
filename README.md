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
  - [Other commands](#other-commands)
- [Installation](#installation)
  - [Update](#update)
  - [Uninstall](#uninstall)
- [Agents](#agents)
  - [Claude Code](#claude-code)
- [Configuration](#configuration)
  - [Project .gitignore](#project-gitignore)
  - [File exclusions](#file-exclusions)
  - [File inclusions](#file-inclusions)
  - [Domain whitelist](#domain-whitelist)
  - [Dependencies](#dependencies)
  - [Container settings](#container-settings)
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

The entire home directory is mounted as a persistent Docker volume (each agent type has separate volume).
This allows for credentials to persist across sandbox instances. On first run, the volume is created automatically.

### Flags

```sh
hole start {agent} {project path} --debug               
hole start {agent} {project path} --dump-network-access  
hole start {agent} {project path} --rebuild              
```

`--debug` sets up the sandbox normally but drops you into an interactive shell for inspecting volumes, network connectivity, and installed packages.

`--dump-network-access` writes a `.hole/logs/network-access-{agent}-{instance id}.log` file to the project directory after the agent exits, containing a sorted list of distinct domains (both allowed and denied).

`--rebuild` forces a fresh build of the sandbox Docker images. Sandbox images are cached per-project for fast startup — use this flag after changing `dependencies`, hook scripts, or when the base agent image needs updating.

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

**Requirements:** `curl` or `wget`, `tar`, [`docker`](https://www.docker.com/get-started/), [`jq`](https://jqlang.github.io/jq/download/), [`jv`](https://github.com/santhosh-tekuri/jsonschema/releases)

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

These are currently supported agents: [Claude Code](https://claude.com/product/claude-code)

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
            "~/.claude/statusline-command.sh": "/home/agent/.claude/statusline-command.sh",
            "~/.claude/settings.json": "/home/agent/.claude/settings.json"
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
            "~/.claude/skills": "/home/agent/.claude/skills"
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
            "~/.claude/settings.json": "/home/agent/.claude/settings.json"
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


## Configuration

Settings are defined in `~/.hole/settings.json` (global) and/or `.hole/settings.json` (per-project). When both exist, they are deep-merged: objects are recursively merged (project values win for scalar conflicts), arrays are concatenated and deduplicated (global items first).

You can find JSON chema definition at `~/.local/share/hole/schema/settings.schema.json`.

### Project .gitignore

It is recommended to add the following paths to your project `.gitignore` to avoid accidentally committing unwanted files:

```
.hole/logs/
```

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

Glob patterns are supported for matching multiple paths at once:

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
      "~/.m2/repository": "/home/agent/.m2/repository",
      "~/.m2/agent-settings.xml": "/home/agent/.m2/settings.xml",
      "~/.m2/agent-toolchains.xml": "/home/agent/.m2/toolchains.xml" // optional, only if you use toolchains
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