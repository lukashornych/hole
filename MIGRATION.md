# Migrating from Hole 1.x to 2.0

Hole 2.0 is a rewrite in Go. It is one static binary instead of a bash tree, and its network
filtering works at the network layer instead of through an HTTP proxy. Most of your setup
carries over unchanged; three settings keys were replaced.

If you only read one thing: **run the installer once, then start a sandbox.** Hole reads your
settings and, if anything needs changing, prints the exact replacement to paste.

- [Upgrading](#upgrading)
- [Settings changes](#settings-changes)
  - [network.domainWhitelist and network.allowedPorts → network.allow](#networkdomainwhitelist-and-networkallowedports--networkallow)
  - [hooks.setup is now an array](#hookssetup-is-now-an-array)
  - [New settings](#new-settings)
- [Behavior changes](#behavior-changes)
- [Troubleshooting](#troubleshooting)

## Upgrading

Run the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
```

`hole update` on a 1.x installation cannot do this for you — it would be replacing a bash
tarball with a binary — so the installer is the one-time step. After that, `hole update`
upgrades in place.

What changes on your machine:

- `~/.local/bin/hole` becomes the binary itself instead of a wrapper script.
- `~/.local/share/hole/` is no longer used. The installer removes it, and so does the binary's
  first run if anything is left.
- **`jq`, `jv`, `sha1sum`, `tar` and `flock` are no longer required.** Only docker or podman
  with the compose plugin.
- `~/.hole/settings.json`, `~/.hole/agents/` and your project `.hole/settings.json` files stay
  where they are.

The first run of 2.0 also cleans up Docker resources 1.x left behind: the `proxy` and `dns`
images (those services no longer exist), `:latest`-tagged agent images, the
`hole-sandbox-docker-cache` volume, old agent-home volumes, and unattached 1.x networks. This
is logged and best-effort; nothing fails the run.

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
| `git.worktreeLinks` | mount related git worktrees (`ro` by default, `rw`, or `off`) |
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
`hole-sandbox-docker-cache` volume is removed on first run. The mirror caches Docker Hub only;
other registries need `network.allow` entries as before.

**Docker Hub now needs an allow entry too.** In 1.x, enabling Docker-in-Docker made Hub reachable
with no `network.allow` entry at all. In 2.0 the mirror is attached to a sandbox only when its
allow-list contains `"docker.io"` or `"*.docker.io"` (or filtering is off with `-u`) — the mirror
reaches Hub over a channel the gateway does not filter, so that access is now something you ask for
rather than a side effect of turning Docker on. Without the entry the sandbox still starts and warns,
and `docker pull nginx` fails while pulls from registries you *have* allowed keep working.

**The Docker-in-Docker sidecar now receives only the file exclusions.** 1.x handed it the agent's
whole mount set — exclusions, `files.include` targets *and* `libraries` — although both its own code
comment and its README described exclusions only. The wider set was never a deliberate decision, and
it mattered: the sidecar is privileged, and a privileged process can remount a read-only bind
read-write, so a `:ro` library reachable from the sidecar was effectively writable.

Nothing changes for the agent, and builds are unaffected — `docker build` and `buildx` stream the
context from the client, so they work against paths the daemon cannot see. The one thing that breaks
is a **bind mount at run time** that points at a library or an included path: `docker run -v
/libs/shared:/x` or an equivalent compose `volumes:` entry no longer resolves inside the sandbox.
The project directory is still mounted at the same absolute path in both containers.

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

**Leftover 1.x resources.** The first run of 2.0 removes them. To be thorough:

```sh
hole destroy      # every Hole Docker resource
```

**Rolling back.** 1.x releases remain on GitHub. Reinstalling one restores the bash tool, but
keep in mind that a settings file migrated to `network.allow` will not validate against it —
keep a copy if you want to move back and forth.
