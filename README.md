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

_Note: any existing sandboxes must be destroyed manually before updating._

### Uninstall

Destroy any running sandboxes first (`hole {agent} destroy {project path}`), then run:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
# or
wget -qO- https://raw.githubusercontent.com/lukashornych/hole/main/uninstall.sh | bash
```

## Sandboxes

Create a new (or start an existing) agent sandbox in the current directory:
```shell
hole {agent} start .
```

Destroy the existing agent sandbox for the current directory:

```shell
hole {agent} destroy .
```

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

## Agents

### Claude

#### Setup

Before you start using Claude sandbox, is good idea to setup long-lived authentication token so you
don't have to login every time you create a new sandbox.

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

Create a new (or start an existing) Claude sandbox in the current directory:
```shell
hole claude start .
```

Destroy the existing Claude sandbox for the current directory:

```shell
hole claude destroy .
```

