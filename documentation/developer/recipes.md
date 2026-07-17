# Recipes

Step-by-step instructions for common development tasks.

## Run Hole from a dev checkout

No build step — run the script directly:

```bash
./hole.sh start claude /path/to/some/project
```

- Without a `version` file in the script directory, the update check and `hole update` are
  disabled (`hole version` prints `development`), so a checkout never tries to replace itself.
- Requirements: docker (or podman) with the compose plugin, `jq`, `jv`, `sha1sum`,
  `curl`/`wget`. Set `HOLE_RUNTIME=podman` to force a runtime.

### Useful flags while developing

```bash
./hole.sh start claude . -d          # bash shell instead of the agent CLI — inspect the sandbox
./hole.sh start claude . -r          # force image rebuild (busts the CACHEBUST layer)
./hole.sh start claude . -n          # write ALLOWED/DENIED domains to .hole/logs/ on exit
./hole.sh start claude . -u          # disable domain filtering entirely
./hole.sh start claude . -- -p "hi"  # pass args to the agent CLI
```

## Debug a failing sandbox

- Generated artifacts (compose override, tinyproxy conf, whitelist, Corefile) live in
  `~/.hole/tmp/run.XXXXXX/` **while the sandbox runs** — the directory is wiped on exit, so
  inspect it from a second terminal.
- `docker compose -p <instance_name> logs proxy|dns|agent` shows service logs;
  the instance name (`hole-sandbox-<project>-<id>`) is printed at startup.
- Proxy denials: run with `-n` and check the `DENIED` lines in the dump, or
  `docker exec <instance>-proxy-1 cat /var/log/tinyproxy/tinyproxy.log`.
- Start with `-d` to get a shell in the agent container and test connectivity manually
  (`curl -v https://example.com` goes through the proxy via the env vars).
- If a run died uncleanly, `hole destroy <path>` removes leftover containers/networks/images
  for that project; a stale sandbox network is also removed automatically on the next start.

## Add a new supported agent

1. Create `agents/<name>/` with:
   - `command.json` — JSON array of argv parts (may reference `$HOME`)
   - `allowed-domains.txt` — tinyproxy regex patterns the CLI needs (escape dots)
   - `install-user.sh` — install the CLI as the agent user; pre-seed config to skip
     first-run prompts if possible
   - `install-root.sh` — only if system-level packages are needed
2. Add the name to `VALID_AGENTS` in `hole.sh`.
3. Add the name to the `container.enabledAgents` enum in `schema/settings.schema.json`.
4. Add the new files to the packaging step in `.github/workflows/release.yml`.
5. Document the agent in the README (`## Agents` section) and update
   [agents.md](agents.md) if the mechanism itself changed.
6. Verify: `./hole.sh start <name> /tmp/some-project -r` and check the CLI starts and can reach
   its API domains (use `-n` to confirm nothing needed is DENIED).

## Add a new settings option

1. Add the property to `schema/settings.schema.json`. The schema is strict
   (`additionalProperties: false`) — without this step every user of the new option fails
   validation at startup.
2. Read the value from the merged settings in `generate_instance_compose()` (or wherever
   appropriate) with `jq -r '... // empty'`. Merging is generic — objects deep-merge with
   project-wins, arrays concatenate + dedupe — so pick the JSON shape accordingly (see
   [configuration — merge semantics](configuration.md#merge-semantics)).
3. Path-valued options must go through `expand_env_vars` / `resolve_host_path`.
4. Follow the [error-handling convention](guidelines.md#error-handling): warn+skip for bad user
   input that is safe to ignore, error+exit otherwise.
5. Document it in the README (`## Configuration`) and in
   [configuration.md](configuration.md).

## Add allowed domains

- Agent CLI needs a new domain → `agents/<agent>/allowed-domains.txt` (regex, escape dots):

  ```
  example\.com
  .*\.example\.com
  ```

- Project-specific domains are user config → `network.domainWhitelist` in `settings.json`
  (plain names, escaping is automatic).
- The default base `proxy/allowed-domains.txt` stays empty by design.

## Add a new source file

Add a `cp` line for it to the "Package release archive" step in
`.github/workflows/release.yml`. Files missing there are absent from installed releases —
the installer downloads the release tarball, not the git repo. Developer documentation and CI
files do not need to be packaged.

## Pre-PR checklist

- [ ] Branch created from `dev`, PR targets `dev`
- [ ] Conventional commit messages (`feat:` bumps the minor version on release)
- [ ] Shell code follows the [bash conventions](guidelines.md#bash-coding-conventions)
- [ ] New source files added to `.github/workflows/release.yml`
- [ ] New/changed settings reflected in `schema/settings.schema.json`
- [ ] README updated for user-facing changes; `documentation/developer/` updated for
      internals
- [ ] Manually verified with `./hole.sh start ... ` (use `-d`/`-n` as needed)
