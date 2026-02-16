# Hole - AI agent sandboxes

CLI tool to create and manage sandboxes for AI agents. It supports limiting file access
and network access.

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