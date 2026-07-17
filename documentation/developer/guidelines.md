# Guidelines

## Bash coding conventions

All shell code in this repository follows these rules:

- Use shell strict mode (`set -euo pipefail`)
- Use local variables in functions; do **not** use global variables to pass data between
  functions (return via stdout and capture with `$()`)
  - Documented exceptions: `CONTAINER_RUNTIME`, `COMPOSE_CMD` and the `_CLEANUP_*` state read by
    the EXIT trap — traps cannot receive arguments
- Always double-quote variables
- Prefer `${VAR}` syntax
- Use lowercase for local variables
- Use `$()` for command substitution
- Use `[[ ]]` for conditionals
- Use arithmetic expansion `(( ))` for math
- Use `getopts` for command-line argument parsing
- Log via the sourced `logger.sh` library (`log_info`, `log_warn`, `log_error`, `log_line`);
  do not use `echo` for logging. Exception: `install.sh` and `uninstall.sh` define their own
  minimal loggers because they must be runnable standalone (piped from curl / from a temp copy).

## Error handling

Two established patterns — pick by whose fault the problem is:

- **User configuration problems that don't compromise the sandbox** (missing excluded path,
  glob with no matches, missing hook script, undefined env var in a path): `log_warn` and skip
  the entry; startup continues.
- **Problems that make the sandbox wrong or unsafe** (invalid settings schema, unknown
  agent/command, missing required tool, invalid `hostGatewayDomains` entry, failed network
  allocation): `log_error` and `exit 1`.
- Cleanup/teardown code never aborts: best-effort with `|| true`, warnings only — every phase
  must get its chance to run.
- Check required external tools with `require_cmd` (from `utils.sh`) before first use.

## Documentation rules

- **User-facing changes** (CLI flags, settings, observable behavior) → document in
  [README.md](../../README.md).
- **Developer-facing changes** (architecture, internals, conventions) → reflect in
  `documentation/developer/`. When implementing or changing any functionality, the relevant
  documentation files HAVE TO be updated in the same change.
- Keep source-code comments limited to what the code cannot express (constraints, non-obvious
  reasons like the Colima/Lima `$HOME` requirement). Do not paste implementation-plan content
  (phases, task lists) into source files; plans live in their own Markdown files.

## Git workflow

- `main`: release branch — every push to `main` triggers the release workflow
  (see [build & release](build-and-release.md))
- `dev`: current development; target for PRs and feature branches
- Feature branches are created from `dev`
- Use [conventional commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`,
  ...). Versioning depends on it: the release workflow's `codacy/git-version` bumps the
  **minor** version on `feat:` commits and the patch version otherwise.

## Release packaging rule

Any new source file needed at runtime **must** be added to the packaging step in
`.github/workflows/release.yml`, otherwise it will be missing from installed releases
(the installer downloads the release tarball, not the git repo). See
[recipes](recipes.md#add-a-new-source-file).
