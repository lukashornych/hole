# Migrating from Hole 1.x to 2.0

Hole 2.0 is a rewrite in Go. It is one static binary instead of a bash tree, and its network
filtering works at the network layer instead of through an HTTP proxy. Most of your setup
carries over unchanged; three settings keys were replaced.

If you only read one thing: **exit your running sandboxes, uninstall 1.x, run the installer once,
then start a sandbox.** Hole reads your settings and, if anything needs changing, prints the exact
replacement to paste.

- [Upgrading](#upgrading)
  - [1. Exit every running 1.x sandbox](#1-exit-every-running-1x-sandbox)
  - [2. Uninstall 1.x](#2-uninstall-1x)
  - [3. Install 2.0](#3-install-20)
  - [What changes on your machine](#what-changes-on-your-machine)
- [Settings changes](#settings-changes)
  - [network.domainWhitelist and network.allowedPorts → network.allow](#networkdomainwhitelist-and-networkallowedports--networkallow)
  - [hooks.setup is now an array](#hookssetup-is-now-an-array)
  - [New settings](#new-settings)
- [Behavior changes](#behavior-changes)
- [Troubleshooting](#troubleshooting)

## Upgrading

Three steps, in this order. `hole update` on a 1.x installation cannot do any of it for you — it
would be replacing a bash tarball with a binary — so this is a one-time manual upgrade. Afterwards
`hole update` upgrades in place.

### 1. Exit every running 1.x sandbox

Let each agent exit normally, so 1.x tears its own sandbox down. **Do not upgrade with a sandbox
still running.** 1.x runs its teardown from an `EXIT` trap inside `hole.sh` — the very script step 2
deletes — so removing it from under a live run leaves that teardown unreliable, and nothing else
will stop those containers. 2.0 will not finish the job either: its first-run cleanup skips a
network that still has containers attached, and its garbage collector skips any sandbox with a
running container or a surviving network — both deliberate, so that a healthy sandbox is never
swept. Leftovers therefore stay until you remove them.

If you find leftovers afterwards, 2.0 can clear them — 1.x used the same `hole-sandbox-` naming, so
`hole destroy` removes 1.x resources too:

```sh
hole destroy                   # every Hole container, network, volume and image, 1.x included
```

### 2. Uninstall 1.x

From the 1.x installation, before installing 2.0:

```sh
hole uninstall
```

This removes `~/.local/share/hole` (the bash tree), the `~/.local/bin/hole` wrapper, and 1.x's
containers, networks, volumes and images — **without asking for confirmation**, unlike 2.0's own
`hole uninstall`. **Your configuration is untouched** — `~/.hole/settings.json`,
`~/.hole/agents/` and project `.hole/settings.json` files are not part of it and carry straight over
to 2.0.

Skipping the step is survivable on the installer path — the installer overwrites the wrapper and
2.0's first run removes the rest. It is **not** survivable on the `go install` path: that leaves the
1.x wrapper in place at `~/.local/bin/hole`, and if that directory comes before Go's bin directory
in your `PATH`, typing `hole` keeps running 1.x (or fails once its bash tree is gone).

### 3. Install 2.0

Either the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
```

or, with a Go 1.25+ toolchain, build it from source instead:

```sh
CGO_ENABLED=0 go install github.com/lukashornych/hole/v2/cmd/hole@latest
```

Both give you the same migration behavior — the cleanup below runs on a `go install` build as well.
The differences are where the binary lands (`$(go env GOBIN)`, or `~/go/bin`, which must be on your
`PATH` — *not* `~/.local/bin`, hence step 2) and that `hole update` refuses on a source build,
telling you to re-run the `go install` command instead. See
[installation](README.md#install-with-go-install) for the details.

### What changes on your machine

- `~/.local/bin/hole` becomes the binary itself instead of a wrapper script — or goes away
  entirely, if you installed with `go install` and the binary now lives in Go's bin directory.
- `~/.local/share/hole/` is no longer used. The installer removes it, and so does the binary's
  first run if anything is left.
- **`jq`, `jv`, `sha1sum`, `tar` and `flock` are no longer required.** Only docker or podman
  with the compose plugin.
- `~/.hole/settings.json`, `~/.hole/agents/` and your project `.hole/settings.json` files stay
  where they are.

The first run of 2.0 also cleans up Docker resources 1.x left behind: the `proxy` and `dns`
images (those services no longer exist), `:latest`-tagged agent images, the
`hole-sandbox-docker-cache` volume, the shared agent-home volumes (dropped back in 1.8, so these
exist only if this machine last ran something older), and unattached 1.x networks. This is logged
and best-effort; nothing fails the run.

## Settings changes

### network.domainWhitelist and network.allowedPorts → network.allow

1.x filtered HTTP and HTTPS with a domain whitelist plus a global list of CONNECT ports. 2.0
filters every protocol and port, so the two keys were replaced by one that says which hosts are
reachable **on which ports**:

```json
// 1.x
{
  "network": {
    "domainWhitelist": ["api.github.com", "registry.npmjs.org"],
    "allowedPorts": [443]
  }
}
```

```json
// 2.0
{
  "network": {
    "allow": [
      "api.github.com:443",
      "*.api.github.com:443",
      "registry.npmjs.org:443",
      "*.registry.npmjs.org:443"
    ]
  }
}
```

Hole prints exactly this translation for your own settings when it finds the old keys, so you
can paste it in.

Two things to know about the translation:

- **The `*.` entries are there for a reason.** 1.x matched whitelist entries as unanchored
  regular expressions, so `example.com` also matched `api.example.com`. 2.0 requires wildcards
  to be explicit, so the migration suggests both forms to preserve reachability. Drop the
  wildcard entries you don't actually need — that is a tightening, and a good one.
- **Ports are per host now**, and `443,80` is the default. `"api.github.com"` is the same as
  `"api.github.com:443,80"`. A database no longer needs a global port opening:
  `"db.example.com:5432"`.

`"allowedPorts": []` in 1.x meant `ConnectPort 0` — nothing could connect at all. There is no
2.0 equivalent because there is nothing to express: leave `network.allow` empty and the sandbox
reaches only each agent's own domains.

New host forms are available too:

```json
{
  "network": {
    "allow": [
      "github.com:22,443",        // git over ssh, no proxy configuration anywhere
      "10.0.0.5:5432",            // a literal address
      "192.168.1.0/24:8080"       // a range
    ]
  }
}
```

### hooks.setup is now an array

So you can bake several scripts into the image, and use globs:

```json
// 1.x
{ "hooks": { "setup": { "script": ".hole/setup.sh" } } }
```

```json
// 2.0
{ "hooks": { "setup": [{ "script": ".hole/setup.sh" }] } }
```

```json
// or, with several scripts run in lexicographic order
{ "hooks": { "setup": [{ "script": ".hole/setup.d/*.sh" }] } }
```

`setupHost`, `prestart` and `cleanupHost` were already arrays and are unchanged — they simply
accept globs now too.

### New settings

Nothing here is required; all of it is opt-in.

| Setting | What it does |
|---|---|
| `profiles` | named overlays selected with `hole start <agent>:<profile>` |
| `agents.<name>.args` | default CLI arguments for an agent |
| `git.worktreeLinks`, `git.worktreePool` | mount related git worktrees (`ro` by default, `rw`, or `off`), and optionally a read-write `<project>-worktrees` pool the agent can create worktrees in |
| `network.subnetPool` | the address range sandbox networks come from (default `10.222.0.0/16`) |
| `container.enabledAgents` | now accepts custom agent names, not just the three builtins |

See the [README](README.md#configuration) for details.

## Behavior changes

**A project path is now required.** `hole start claude` used to silently mean the current
directory. Write `hole start claude .` when that is what you want.

**`hole destroy <path>` honors the path.** In 1.x the positional landed in the agent slot and
the *current* directory's project was destroyed instead; `hole destroy .` only worked by
accident.

**`network.hostGatewayDomains` entries now require a `:port,port` suffix.** A 1.x-style port-less
entry (`"mydb.local"`) fails validation at startup with a message naming it — write
`"mydb.local:5432"` instead. The firewall matches the host gateway *address* rather than the name,
so a port-less entry opened every TCP and UDP port on your machine for the sandbox's lifetime.

**No more proxy environment variables.** `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` are gone from the
sandbox, because filtering no longer happens at the HTTP layer. Anything that needed proxy
awareness can drop it:

```xml
<!-- ~/.m2/agent-settings.xml — delete this block -->
<proxies>
  <proxy>
    <id>http-internet</id>
    <host>proxy</host>
    <port>8888</port>
  </proxy>
</proxies>
```

The upside is that tools which never honored those variables now work: ssh, git over ssh,
database clients, raw sockets, UDP and QUIC — as long as you allow the host and port.

**Failures look different.** A name you have not allowed does not resolve, so you get an
immediate DNS error instead of a proxy timeout. That is intentional: fast and obvious beats
slow and mysterious.

**Gemini's built-in domains are narrower.** 1.x listed the unanchored regex `googleapis\.com`,
which matched every host under it — `storage.googleapis.com` included, so any sandbox with gemini
enabled could read from and write to arbitrary Google Cloud Storage buckets. 2.0 allows only
`cloudcode-pa.googleapis.com` (login with Google), `generativelanguage.googleapis.com`
(`GEMINI_API_KEY`) and `oauth2.googleapis.com` (token refresh). Both auth paths and normal use are
unaffected; Vertex AI now needs `network.allow` entries (`aiplatform.googleapis.com` plus the
regional host), and gemini's usage telemetry (`play.googleapis.com`) is denied, which shows up in a
`-n` dump and changes nothing else.

**`-r`/`--rebuild` is rarely needed.** Images are tagged with a hash of everything that affects
their content, so changing a setting that matters rebuilds automatically. Use `-r` only to pick
up newer apt packages or agent CLI versions.

**Images are shared between projects.** Projects whose settings do not change the image content
use one shared image, so a rebuild in one benefits all of them. The start banner names the image
and says whether it is shared or project-specific, and why.

**The Docker-in-Docker cache changed.** 1.x seeded a per-instance volume from a shared cache
volume and synced it back on exit. 2.0 runs a long-lived pull-through mirror instead, so the
first pull after upgrading re-downloads and later ones come from the mirror. The old
`hole-sandbox-docker-cache` volume is removed on first run. The mirror container is stopped once
the last sandbox exits and restarted with its cache by the next start, so no Hole container is
left running between sessions. The mirror caches Docker Hub only; other registries need
`network.allow` entries as before.

**Docker Hub now needs an allow entry too.** In 1.x, enabling Docker-in-Docker made Hub reachable
with no `network.allow` entry at all. In 2.0 the mirror is attached to a sandbox only when its
allow-list contains `"docker.io"` or `"*.docker.io"` (or filtering is off with `-u`) — the mirror
reaches Hub over a channel the gateway does not filter, so that access is now something you ask for
rather than a side effect of turning Docker on. Without the entry the sandbox still starts and warns,
and `docker pull nginx` fails while pulls from registries you *have* allowed keep working.

**The Docker-in-Docker sidecar no longer receives `files.include` targets.** 1.x handed it the
agent's whole mount set — exclusions, `files.include` targets *and* `libraries` — although both its
own code comment and its README described exclusions only. Exclusions and `libraries` are still
mirrored, as in 1.x; only the included paths are not. A single file like `~/.npmrc` has no plausible
use as a nested bind mount, so there is no reason to hand it to the privileged sidecar.

Nothing changes for the agent, and builds are unaffected — `docker build` and `buildx` stream the
context from the client, so they work against paths the daemon cannot see. The one thing that breaks
is a **bind mount at run time** that points at an included path: `docker run -v
/opt/npmrc:/x` or an equivalent compose `volumes:` entry no longer resolves inside the sandbox. Move
the entry to `libraries` if you need it there. The project directory is still mounted at the same
absolute path in both containers.

**Project names changed, so cached images from an earlier 2.0 build are orphaned.** The hash
suffix in `hole-sandbox-<project>-<hash>` is now taken over the project path as written, not over
a sanitized version of it. It had to be: sanitizing dropped the characters that tell
`~/my_project` and `~/myproject` apart, so the two shared one name — and `hole destroy` on either
one force-removed the *running* containers, networks and images of the other.

Nothing is required of you. Images under the old names are reclaimed by the usual image
collection, and the first sandbox after upgrading rebuilds. Do stop running sandboxes before
upgrading, though: an instance started under the old name is no longer found by
`hole destroy <path>`, and has to be removed with `hole destroy` (no path) or by name.

One consequence to know about: on a case-insensitive filesystem (macOS by default), `/Users/me/x`
and `/Users/me/X` now name two sandboxes for one directory instead of one. Two sandboxes are
harmless; the cross-project destroy was not.

**`$VAR` in `environment` values and configured agent `args` still works, but Hole resolves it
now.** 1.x wrote those values into the compose file unescaped, so Docker Compose interpolated them
from your environment as a side effect. 2.0 escapes `$` so a literal one in a path or a password
survives, and expands these two settings itself instead — same result, minus the surprise. Two
differences worth knowing: `$HOME` resolves to the *sandbox* home rather than yours, and an
undefined variable stays literal with a warning instead of becoming an empty string.

**`cleanupHost` hooks run without a TTY.** Teardown happens in a detached supervisor so it
survives a closed terminal, which means those scripts cannot prompt. Their output goes to the
run log.

**A project's own settings now need your confirmation before they reach your host.** 1.x trusted
`<project>/.hole/settings.json` completely: cloning a repository and starting a sandbox in it ran
that file's `hooks.setupHost` on your machine, mounted whatever `files.include` and `libraries`
named, and could switch on the privileged Docker-in-Docker sidecar — all without a word. 2.0 shows
you what a project asks for and asks once, remembering the answer in `~/.hole/trust.json`. Only
settings whose effect leaves the sandbox are gated (`hooks.setupHost`, `hooks.cleanupHost`,
`hooks.setup`, `files.include`, `libraries`, `container.docker`, `dependencies`,
`network.hostGatewayDomains`, `network.allow`); everything else is confined to the container and
never prompts.

The two network keys are the ones to watch: a project file that used to start silently because it
contained nothing but `network.allow` now needs a confirmation. Every destination a repository adds
is also somewhere the sandbox can send that repository's contents, which is what the default-deny
policy exists to prevent.

Your own `~/.hole/settings.json` is never gated, so a global configuration keeps working
untouched. What does change: a **non-interactive** run — CI, a piped invocation — cannot be asked,
so it fails instead of granting silently. Add `--trust-project` to accept the project's current
requests up front — bearing in mind it accepts whatever the file asks for at that moment, host
hooks included. See [project trust](README.md#project-trust).

**The `-n` network-access dump moved out of the project.** 1.x wrote it to
`<project>/.hole/logs/`, inside the sandbox's own read-write mount; 2.0 writes it to
`~/.hole/logs/<project>/network-access-<agent>-<id>.log`, which the sandbox cannot reach. Nothing
else was ever written into `<project>/.hole/`, so if you added `.hole/logs/` to a project's
`.gitignore` purely for these dumps, you can drop that line.

**New: `hole list`** shows what is running, and per-run debug logs are written to
`~/.hole/logs/`.

## Troubleshooting

**"uses settings that were removed in Hole 2.0"** — this is the migration error. It names the
keys, shows the replacement, and does not start the sandbox. Paste the suggested block and try
again.

**Something in the sandbox cannot reach the internet.** Run with `-n`:

```sh
hole start claude . -n
```

On exit, `~/.hole/logs/<project>/network-access-claude-<id>.log` lists every domain the sandbox
resolved (`ALLOWED`) and every one it was refused (`DENIED`). The `DENIED` lines are your
missing `network.allow` entries. Note that a tool connecting to a hardcoded IP never appears
there, because it never asks DNS.

**A tool that worked in 1.x now fails on a non-standard port.** Ports are per host now. Add the
port: `"db.example.com:5432"`.

**Sandbox networks collide with a VPN.** Point Hole's pool somewhere else:

```json
{ "network": { "subnetPool": "10.99.0.0/16" } }
```

**`hole version` still reports 1.x after a `go install` upgrade.** The 1.x wrapper is still at
`~/.local/bin/hole` and wins on `PATH`. Remove it (`rm ~/.local/bin/hole`, or run 1.x's
`hole uninstall`) and check with `which hole`.

**Leftover 1.x resources.** The first run of 2.0 removes them. To be thorough:

```sh
hole destroy      # every Hole Docker resource
```

**Rolling back.** 1.x releases remain on GitHub. Reinstalling one restores the bash tool, but
keep in mind that a settings file migrated to `network.allow` will not validate against it —
keep a copy if you want to move back and forth.
