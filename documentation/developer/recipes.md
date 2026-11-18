# Recipes

## Build and run from a checkout

```sh
make build          # static binary at ./hole
./hole start claude /path/to/project
./hole start claude . -d          # bash shell instead of the agent CLI
./hole start claude . -r          # force an image rebuild
./hole start claude . -n          # dump resolved/refused domains on exit
```

A checkout build reports `hole development`, which skips update checks and refuses to
self-update.

To try a stamped version: `VERSION=2.0.0 make build`.

## Run the tests

```sh
make test     # unit tests, no container runtime needed
make itest    # integration tests, needs a real docker/podman daemon
make e2e      # end-to-end sandbox runs, slow (builds images)
make lint     # gofmt + go vet under all build tags + golangci-lint if installed
make golden   # regenerate golden compose/gateway artifacts after an intended change
```

What each suite covers, the prerequisites they need, the traps (`make itest` skips silently without
a daemon, `make test` is not `-race`) and how to match CI locally:
[development environment](development.md#the-four-suites).

## Add a supported agent

1. Create `assets/agents/<name>/` with `command.json`, `allow.txt`, and the install scripts it
   needs. The embed directive already covers the directory, so nothing else needs registering.
   Install scripts end by cleaning their package manager's cache.
2. Add `update-user.sh` (and `update-root.sh` if the agent has system-level parts) plus
   `version.json`, so the agent takes part in [in-place updates](agents.md#in-place-agent-updates).
   If the command names a pinned Node by path, `version.json` names the same one.
   Such an agent also gets a `command-bare.json` naming that same Node (without the yolo flags), so
   its CLI is callable by name inside the sandbox ([agents](agents.md#plugin-contract)).
3. Add it to the README's agent list, with its authentication mount, and its files to
   `TestEmbeddedTreeIsComplete`.
4. `make test` — the registry test asserts every builtin has a parseable command, a non-empty
   allow list, at least one install script, an update script and a valid `version.json`.

There is no `VALID_AGENTS` list to update any more, and no schema enum: agent names are validated
against the registry at runtime.

## Add a settings option

1. Add it to `assets/schema/settings.schema.json`, inside `$defs/settings` so profiles get it too.
2. Add the field to the typed model in `internal/config/settings.go`.
3. Use it. If it is path-valued, resolve it through `hostenv.Host.ResolveHostPath`.
4. Decide whether it affects the image. If it does, add it to the canonical config in
   `internal/image` **and** the table in [configuration](configuration.md#image-identity-and-scope),
   or cached images will go stale.
5. Decide whether its effect leaves the sandbox — host code, a host path, a privileged container,
   or anything running during the image build. If it does, add it to `capabilities` in
   `internal/trust`, or a project file will be able to set it without the user's consent
   ([configuration](configuration.md#project-trust)).
6. Add a valid and an invalid example to the schema tests, and a merge test if its merge behavior
   is interesting.
7. Document it in the README.

## Add a setting to the runtime registry

`hole modify` can change only the settings in `modifiableSettings` (`internal/sandbox/modify.go`).
To add one:

1. Record its start-time value in `state.Instance` (canonical strings, written by `Start` before
   the service that uses it exists) and give it a runtime list field next to `RuntimeAllow`.
2. Add a `ModifiableSetting` entry: `Canonical` parses and validates a single value (it is what
   equality, state and listings use), `base`/`runtime` point at the two state fields, and
   `Operations` lists what it supports (`list-blocked` only with `HasBlocked`).
3. Make the apply re-render it: `parseGatewayConfig` and `renderApplyPayload` build the whole
   gateway configuration from state. A setting that does not live in the gateway needs its own
   in-place apply path, with the same rules — validate before changing anything, write state before
   applying, write the previous lists back on failure.
4. Extend `validateRuntime` with rules that need the whole configuration, and `runtimeEntryCount`
   (`hole list`) if its entries should be counted.
5. Add cases to `modify_test.go` and the CLI parsing table, and document it in the README section
   *Changing the network of a running sandbox*.

## Add a runtime asset

Put it under `assets/` beneath an existing embed directive, and materialize it where it is needed
(`internal/sandbox` writes the build contexts and gateway configuration). If it changes image
content, remember that the embedded-assets digest is part of the image tag, so a change invalidates
cached images automatically.

## Debug a failing sandbox

```sh
./hole start claude . -d                      # shell inside the sandbox
cat ~/.hole/logs/run-*.log | tail -50         # every runtime command, with timings
tail -n1 ~/.hole/logs/run-*/*.log             # how every hook script of each run ended
HOLE_BUILD_PROGRESS=plain ./hole start claude . -r   # raw image build output on the console
hole list                                     # what is still running
```

From a debug shell inside the sandbox:

```sh
getent hosts example.com          # is the name allowed? NXDOMAIN means "not in the policy"
ip route                          # default route should be the gateway address
env | grep -i proxy               # should be empty: there is no proxy any more
```

From the host, against the gateway container:

```sh
docker logs <instance>-gateway-1                       # CoreDNS query log, interface detection
docker exec <instance>-gateway-1 nft list ruleset      # the live firewall, incl. denial counters
docker exec <instance>-gateway-1 nft list table inet hole   # sets (b_*/r_*) with the addresses dnsmasq recorded
cat ~/.hole/tmp/run.*/gateway-conf/Corefile            # the generated policy (while running)
```

Triage strings worth recognising:

| Message | Cause |
|---|---|
| `uses settings that were removed in Hole 2.0` | a 1.x settings file; the error shows the replacement |
| `unknown profile '<name>'` | typo or wrong file; the error lists what each file defines |
| `subnet pool ... exhausted: N of M` | too many sandboxes, or `network.subnetPool` is too small |
| `this dnsmasq build has no nftset support` | the gateway base image lost the feature — see [networking](networking.md#base-image-constraint-dnsmasq-needs-nftset) |
| `could not identify sandbox and internet interfaces` | the gateway saw unexpected addresses; check `HOLE_SANDBOX_SUBNET` |
| `left resources behind` | teardown could not remove something; it names each one |
| `No such container` during startup | a teardown ran while the sandbox was still starting — check the watchdog's records (`component=watchdog`) in the run log |
| `the <service> container no longer exists` | same cause, reported from the CLI side |

## Pre-PR checklist

- `make lint && make test && make itest` pass; `make e2e` if the change touches startup, the
  gateway or teardown.
- Golden files regenerated and their diff reviewed.
- Schema updated for any new setting, with tests.
- README updated for user-facing changes; these docs updated for internals; MIGRATION.md updated if
  1.x behavior changed.
- Decision record written if the work clears the bar in `.claude/rules/adr.md`, its plan deleted,
  and the index regenerated with `scripts/generate-adr-index.sh`.
- Conventional commit subject (`feat:` bumps the minor version).
