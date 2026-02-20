# Hole - AI agent sandboxes

CLI tool to create and manage sandboxes for AI agents. It supports limiting file access
and network access.

## Installation

**Requirements:** `curl` or `wget`, `tar`, and `docker`.

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

Run the same install command again — the installer detects an existing installation, removes it, and reinstalls from the latest `main`.

_Note: any running sandboxes should be exited before updating._

### Uninstall

Exit any running sandboxes first (sandboxes are automatically destroyed on exit), then run:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
# or
wget -qO- https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
```

## Sandboxes

Create an agent sandbox in the current directory:
```shell
hole {agent} start .
```

The sandbox is fully destroyed when you exit the agent CLI. Multiple sandboxes can run simultaneously for the same project — each gets a unique instance ID.

### Configuration

#### File exclusions

You can hide project files and folders from the agent by creating a `.hole/settings.json` in your project root:

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

Files are mounted as `/dev/null` and directories as empty anonymous volumes inside the container, making them inaccessible to the agent. Trailing slashes are stripped automatically. Non-existent paths are skipped with a warning.

#### File inclusions

You can mount additional host files or directories into the sandbox via `files.include` in `.hole/settings.json`. Keys are host paths, values are absolute container paths:

```json
{
  "files": {
    "include": {
      "./shared-config": "/workspace/shared-config",
      "/home/user/data": "/data",
      "~/.npmrc": "/home/claude/.npmrc"
    }
  }
}
```

Host paths are resolved as follows: `~/` is expanded to `$HOME`, relative paths are resolved against the project directory, and absolute paths are used as-is. Non-existent host paths are skipped with a warning. Container paths must be absolute (start with `/`).

#### Domain whitelist

By default, agents can only reach a small set of domains required for their operation (e.g., `api.anthropic.com` for Claude). To allow access to additional domains, add a `network.domainWhitelist` array to `.hole/settings.json`:

```json
{
  "network": {
    "domainWhitelist": [
      "registry.npmjs.org",
      "api.github.com"
    ]
  }
}
```

Use plain domain names — dots are auto-escaped for the proxy filter. Default domains are always included; project-specific domains are appended on top.

#### Dependencies

Additional apt packages can be installed at container startup via the `dependencies` array in `.hole/settings.json`:

```json
{
  "dependencies": [
    "python3",
    "build-essential",
    "htop"
  ]
}
```

Packages are installed via `apt-get install` before the agent CLI starts. When dependencies are specified, Ubuntu apt repository domains are automatically added to the proxy whitelist. Global and project dependencies are merged using the same concatenation and deduplication strategy as other array settings.

#### Global settings

You can define global defaults in `~/.hole/settings.json` so they apply to every project without repeating them:

```json
{
  "files": {
    "exclude": [".env", ".env.local"],
    "include": {
      "~/.npmrc": "/home/claude/.npmrc"
    }
  },
  "network": {
    "domainWhitelist": [
      "registry.npmjs.org"
    ]
  }
}
```

Global and project settings are deep-merged: arrays are concatenated and deduplicated (global items first), while project scalar values take precedence. For example, if the global file excludes `[".env", "node_modules"]` and the project excludes `["node_modules", "dist"]`, the merged result is `[".env", "node_modules", "dist"]`.

#### Network access log

To see which domains the agent accessed during a session, pass the `--dump-network-access` flag:

```shell
hole claude start . --dump-network-access
```

After the agent exits, a `claude-network-access-{id}.log` file is written to the project directory containing a sorted list of distinct domains (both allowed and denied requests). The `{id}` is the unique instance ID assigned to that sandbox session.

## Agents

### Claude

#### Setup

Before you start using Claude sandbox, is good idea to setup long-lived authentication token so you
don't have to login every time you create a new sandbox.

todo lho either install on host or login each time you start sandbox

todo lho only needed for max

1. install [Claude Code](https://claude.com/product/claude-code) locally
2. run `claude setup-token`
3. login to Claude
4. after successful login, you should be redirected back to terminal with the following message:

  ```
  ✓ Long-lived authentication token created successfully!
                                                                                                                                                           
  Your OAuth token (valid for 1 year):                                                                                                                     
                                     
  sk-ant-..........
  
  Store this token securely. You won't be able to see it again.
  
  Use this token by setting: export CLAUDE_CODE_OAUTH_TOKEN=<token>
  ```

5. store the OAuth token as your environment variable (`.bashrc` on Linux, `.zshrc` on macOS)

  ```
  export CLAUDE_CODE_OAUTH_TOKEN="sk-ant-........"
  ```

6. start Claude Code locally and setup and login again

#### Use

Create a Claude sandbox in the current directory:
```shell
hole claude start .
```

The sandbox is fully destroyed when you exit the CLI.

