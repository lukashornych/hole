# Hole

## What is Hole?

Hole is a CLI tool for running AI agents in isolated Docker sandboxes.

Running AI agents directly on your host machine is risky — they have access to your filesystem, network, and credentials. Built-in agent sandboxes (e.g. Claude Code's) can potentially be bypassed by the agent itself, since the agent controls the process.

Hole provides true isolation through:

- **Network control** — the sandbox has no route to the internet except through Hole's filtering gateway, which denies everything you have not allowed, on every protocol and port
- **File access control** — project files are mounted into the container, with configurable exclusions (e.g. `.env`, `node_modules`) hidden from the agent
- **Containerized execution** — the agent runs as a non-root user inside a Docker container that is destroyed on exit

> **Upgrading from 1.x?** Hole 2.0 is a single binary with no `jq`/`jv` dependency, and a few
> settings keys changed. See [MIGRATION.md](MIGRATION.md) — Hole also tells you exactly what to
> change the first time it reads an old settings file.

Table of contents:

- [What is Hole?](#what-is-hole)
- [Usage](#usage)
  - [Flags](#flags)
  - [Passing arguments to the agent](#passing-arguments-to-the-agent)
  - [Other commands](#other-commands)
- [Installation](#installation)
  - [Supported Docker runtimes](#supported-docker-runtimes)
  - [Update](#update)
  - [Uninstall](#uninstall)
- [Agents](#agents)
  - [Claude Code](#claude-code)
  - [Gemini CLI](#gemini-cli)
  - [Codex CLI](#codex-cli)
  - [Custom agents](#custom-agents)
- [Configuration](#configuration)
  - [Project trust](#project-trust)
  - [File exclusions](#file-exclusions)
  - [File inclusions](#file-inclusions)
  - [Libraries](#libraries)
  - [Git worktrees](#git-worktrees)
  - [Network access](#network-access)
  - [Host gateway domains](#host-gateway-domains)
  - [Subnet pool](#subnet-pool)
  - [Dependencies](#dependencies)
  - [Container settings](#container-settings)
  - [Docker-in-Docker](#docker-in-docker)
  - [Environment variables](#environment-variables)
  - [Agent arguments](#agent-arguments)
  - [Hooks](#hooks)
  - [Profiles](#profiles)
  - [Configuration examples](#configuration-examples)
- [Logs](#logs)

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

The project path is required. Hole builds the sandbox image if needed, starts the gateway and the agent, and attaches your terminal to the agent CLI. When the agent exits, everything is destroyed.

Run without a terminal — from a script, a CI job, or with the output piped — and the agent gets no TTY and no open stdin, since there is no keyboard to forward. You still get its output and its exit code. An agent that waits for input sees end-of-input and exits instead of hanging, which also means `-d` opens a shell that closes immediately: to poke around a sandbox, run it from a terminal.

### Flags

| Flag | Description |
|---|---|
| `-d`, `--debug` | Open a bash shell instead of the agent CLI, for inspecting the sandbox |
| `-n`, `--dump-network-access` | After the agent exits, write the domains the sandbox resolved (and those it was refused) to `~/.hole/logs/{project}/network-access-{agent}-{id}.log` |
| `-r`, `--rebuild` | Force a rebuild of the sandbox images |
| `-u`, `--unrestricted-network` | Disable egress filtering; allow all network access |
| `--with-docker` | Enable the Docker-in-Docker sidecar |
| `--trust-project` | Accept whatever the project's own `.hole/settings.json` asks for beyond the sandbox — host access and network widening alike — without being asked, and remember it; see [project trust](#project-trust) |
| `--library PATH[:MOUNT][:rw]` | Mount an extra directory (repeatable); defaults to `/libs/{basename}`, read-only unless `:rw` |
| `--` | Everything after this is passed verbatim to the agent CLI |

`-r` is only needed to refresh *versions* (latest apt packages, latest agent CLIs). Changing a
setting that affects the image produces a new image automatically.

### Passing arguments to the agent

```sh
hole start claude . -- -p "explain this function"
hole start claude . --rebuild -- --output-format stream-json
```

Arguments after `--` reach the agent exactly as you typed them. `-d` cannot be combined with them, since debug mode replaces the agent command with a shell.

### Other commands

```sh
hole list                      # show running sandboxes
hole destroy                   # destroy ALL Hole Docker resources
hole destroy .                 # destroy resources for the current project
hole destroy /path/to/project  # destroy resources for a specific project
hole version                   # print the installed version
hole update                    # upgrade to the latest release
hole uninstall                 # remove Hole's Docker resources and the binary
hole help                      # usage
```

`hole list` shows the instance ID, agent (and profile), project path, uptime, whether Docker-in-Docker is enabled, the sandbox network, and which settings files were merged.

## Installation

Hole is a single static binary. Install it with:

```sh
curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
```

The installer detects your OS and architecture, downloads the release binary, verifies its
checksum, and installs it to `~/.local/bin/hole`. Make sure that directory is on your `PATH`.

Upgrading from 1.x is not just this command: exit your running sandboxes and uninstall 1.x first —
see [upgrading](MIGRATION.md#upgrading).

Requirements: **docker or podman with the compose plugin**. That is all — Hole embeds
everything else it needs.

Supported platforms: Linux (amd64/arm64, including WSL) and macOS (Intel/Apple Silicon).

### Install with `go install`

With a Go 1.25+ toolchain you can build Hole from source instead of downloading a release:

```sh
CGO_ENABLED=0 go install github.com/lukashornych/hole/v2/cmd/hole@latest
```

The binary lands in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` (usually `~/go/bin`) when `GOBIN`
is unset — make sure that directory is on your `PATH`, and note it is *not* where the installer
puts Hole, so having both means two `hole` binaries and `PATH` order decides which one runs.
No other tooling is needed — every runtime asset is embedded in the binary. `CGO_ENABLED=0` is what
keeps it static like the released binary; without it, a machine with a C toolchain links it against
the system libc.

Pin an exact release with its tag (`@v2.0.0`) or an arbitrary state with a commit SHA. The `/v2` in
the path is the module's major version, not a typo — Go requires it for a 2.x module, and it stays
there unchanged for every 2.x release, so `@latest` gives you the newest 2.x (a future 3.0 would
live at `/v3` and `@latest` here would never cross to it).

Such a build knows which version it is — `hole version` prints e.g. `2.0.0 (go install)` — and
behaves like a release install in every respect but one: **`hole update` refuses**, because replacing
a binary you built from source with a downloaded one is not its job. It still tells you when a newer
release exists, with the command that upgrades you:

```
A new version of hole is available: 2.1.0 (installed: 2.0.0) — upgrade with:
  go install github.com/lukashornych/hole/v2/cmd/hole@latest
```

An install pinned to a commit (`@<sha>`) has no comparable version number, so it gets no such notice.
`hole uninstall` works either way — it removes whichever binary is running, wherever `go install` put
it.

### Supported Docker runtimes

Hole works with Docker Desktop, OrbStack, Colima, Rancher Desktop and rootless Podman. Set
`HOLE_RUNTIME=podman` to force a runtime when both are installed.

The sandbox network is created from Hole's own address pool (`10.222.0.0/16` by default), so
Docker's default pools stay untouched. If that range collides with a VPN or LAN of yours, see
[subnet pool](#subnet-pool).

### Update

```sh
hole update
```

Hole compares your version against the latest release, verifies the checksum of the new binary,
and replaces itself in place. Every `hole start` also does a silent one-second check and tells
you when a newer version exists.

### Uninstall

```sh
hole uninstall
```

Removes Hole's containers, networks, volumes and images, then the binary. Your settings, custom
agents and logs in `~/.hole` are only removed if you confirm.

## Agents

Supported out of the box: [Claude Code](https://claude.com/product/claude-code), [Gemini CLI](https://github.com/google-gemini/gemini-cli), [Codex CLI](https://github.com/openai/codex). You can also add [your own](#custom-agents).

All enabled agents are installed into one sandbox image; the agent you name on the command line only decides what starts.

### Claude Code

```shell
hole start claude .
```

#### Authentication

You can authenticate with any available method. To stay authenticated across sandbox instances, add this [inclusion](#file-inclusions) to `settings.json`:

```json
{
  "files": {
    "include": {
      "~/.claude": "~/.claude"
    }
  }
}
```

This keeps your Claude settings in sync between sandboxes and your host system. If you also run Claude on the host with different settings, mount the sandbox's `~/.claude` from another host folder:

```json
{
  "files": {
    "include": {
      "~/hole/agents/claude": "~/.claude"
    }
  }
}
```

The **important** part is that the host folder exists before starting the sandbox.

#### Adding marketplaces

Marketplaces are usually added via SSH repositories, but you generally don't want to give your SSH keys to the agent. You can add them over HTTPS instead:

```shell
/plugin marketplace add https://github.com/anthropics/claude-plugins-official.git
```

Allow the marketplace domain in [network access](#network-access). For a private marketplace you will also need authentication, usually a personal access token:

```shell
/plugin marketplace add https://{username}:{pat}@gitlab.mydomain.com/internal/claude-marketplace.git
```

### Gemini CLI

```shell
hole start gemini .
```

#### Authentication

You can authenticate with any available method. To stay authenticated across sandbox instances, add this [inclusion](#file-inclusions) to `settings.json`:

```json
{
  "files": {
    "include": {
      "~/.gemini": "~/.gemini"
    }
  }
}
```

This keeps your Gemini settings in sync between sandboxes and your host system. If you also run Gemini on the host with different settings, mount the sandbox's `~/.gemini` from another host folder:

```json
{
  "files": {
    "include": {
      "~/hole/agents/gemini": "~/.gemini"
    }
  }
}
```

#### Network access

Gemini is allowed `cloudcode-pa.googleapis.com` (login with Google), `generativelanguage.googleapis.com` (`GEMINI_API_KEY`) and `oauth2.googleapis.com` (token refresh) — not `*.googleapis.com`, which would also open `storage.googleapis.com` and every other Google API. Two consequences, both expected:

- A `-n` dump shows `DENIED play.googleapis.com` (usage telemetry) and may show `DENIED www.googleapis.com` (caching your Google account ID, which the CLI logs and moves past). Neither affects the CLI.
- Vertex AI needs its host allowed explicitly, e.g. `"network": {"allow": ["aiplatform.googleapis.com", "europe-west1-aiplatform.googleapis.com"]}`.

The **important** part is that the host folder exists before starting the sandbox.

### Codex CLI

```shell
hole start codex .
```

#### Authentication

You can authenticate with any available method. To stay authenticated across sandbox instances, add this [inclusion](#file-inclusions) to `settings.json`:

```json
{
  "files": {
    "include": {
      "~/.codex": "~/.codex"
    }
  }
}
```

This keeps your Codex settings in sync between sandboxes and your host system. If you also run Codex on the host with different settings, mount the sandbox's `~/.codex` from another host folder:

```json
{
  "files": {
    "include": {
      "~/hole/agents/codex": "~/.codex"
    }
  }
}
```

The **important** part is that the host folder exists before starting the sandbox.

### Custom agents

Any directory under `~/.hole/agents/<name>/` becomes an agent you can start:

```
~/.hole/agents/my-agent/
  command.json       # required — startup command as a JSON array of argv parts
  allow.txt          # required — domains the agent needs (see below)
  install-root.sh    # optional — runs as root during the image build
  install-user.sh    # optional — runs as the sandbox user during the image build
```

```json
// command.json
["my-agent", "--yolo"]
```

```
# allow.txt — <host>[:<port>[,<port>...]], ports default to 443,80
api.my-agent.example
*.my-agent.example
```

Names must match `^[a-z0-9][a-z0-9-]*$`, and a name that collides with a builtin agent is an error rather than a silent override. Custom agents work everywhere builtins do, including `container.enabledAgents`. Editing a custom agent's files rebuilds the image automatically.

## Configuration

Two optional files, sharing one schema:

- `~/.hole/settings.json` — global defaults
- `<project>/.hole/settings.json` — per-project settings

They are merged: objects deep-merge with the project winning, arrays concatenate (global first) and deduplicate, and scalars are overridden by the project.

Add the schema reference for editor completion:

```json
{
  "$schema": "https://raw.githubusercontent.com/lukashornych/hole/main/assets/schema/settings.schema.json"
}
```

All path-valued settings support `$VAR`/`${VAR}` expansion, `~/` (your home on the host side, the sandbox home on the container side), and paths relative to the project directory.

### Project trust

`~/.hole/settings.json` is yours. A project's `.hole/settings.json` is *repository content* — and some settings reach outside the sandbox, which is exactly what you are pointing an agent at a repository to avoid. So the first time a project asks for one of them, Hole shows you what it wants and asks:

```
  The project's own settings ask for access beyond the sandbox:
  /home/you/repo/.hole/settings.json

    hooks.setupHost — runs a script on your host before the sandbox is created
        .hole/setup-host.sh
    files.include — mounts host paths into the sandbox
        ~/.ssh -> ~/.ssh
    network.allow — widens the sandbox's network allow-list
        uploads.example.com

  Trust them only if you trust this repository's contents.

  Trust this project? [y/N]
```

Answering no starts nothing at all — no container, and none of the scripts above. The settings that need confirmation are the ones whose effect leaves the sandbox:

| Setting | What the project is asking for |
|---|---|
| `hooks.setupHost`, `hooks.cleanupHost` | run a script **on your host**, as you |
| `files.include`, `libraries` | mount host paths into the sandbox |
| `container.docker` | add the privileged Docker-in-Docker sidecar |
| `git.worktreePool` | create a worktree directory next to your project and mount it read-write |
| `network.hostGatewayDomains` | reach services running on your host |
| `hooks.setup`, `dependencies` | run during the image build, which uses your host's network rather than the gateway |
| `network.allow` | widen the network allow-list — every destination the sandbox may reach is also somewhere it can send the project's contents |

Everything else in a project file is confined to the sandbox and never prompts — `files.exclude` only takes access away, and `environment`, `agents.*.args`, `container.baseImage`, `hooks.prestart` and `network.subnetPool` act inside the container, which is the boundary. `git.worktreeLinks` is not gated either: it mounts checkouts of the repository you already pointed Hole at, and creates nothing.

A yes is remembered in `~/.hole/trust.json`, keyed by project path and by *what* you accepted: editing an ungated setting later changes nothing, but a project that starts asking for more asks again. Delete the file (or the project's entry) to be asked afresh.

Without a terminal — a CI job, a piped run — there is nobody to ask, so an untrusted project fails to start instead of being granted silently. Pass `--trust-project` to accept its current requests up front:

```sh
hole start claude . --trust-project -- -p "run the test suite"
```

The flag accepts *whatever the file asks for at that moment* — network keys, host mounts, `hooks.setupHost`, all of it. A CI script that carries it because the project only widens `network.allow` today also pre-approves a host hook the repository adds tomorrow.

### File exclusions

Hide files and directories from the agent:

```json
{
  "files": {
    "exclude": [
      ".env",
      ".env.*",
      "secrets",
      "**/*.pem"
    ]
  }
}
```

Files are replaced with `/dev/null` and directories with an empty directory, so the agent sees nothing. Patterns support `*`, `?`, `[...]` and `**` (recursive). Exclusions are mirrored onto the Docker-in-Docker sidecar, so a container started inside the sandbox cannot bind-mount its way to them either.

Three things to know about how patterns are matched:

- **A pattern matching nothing is a warning, not an error.** That is deliberate: exclusions are meant to be written once in your global settings for files only *some* projects have (`.env`, `**/*.pem`), and erroring would make one shared default refuse to start every project without them. The cost is on you — a typo hides nothing and only warns, so check the warnings when a project holds something that matters.
- **Patterns do not follow symlinked directories**, matching bash `globstar`: if `secrets` is a symlink, `secrets/**` matches nothing. Name the link itself (`"secrets"`) and the whole directory is hidden.
- **`files.exclude` does not reach into `files.include` or library mounts.** Patterns apply to the project directory, and separately to each library's own mount (via that library's `.hole/settings.json`). An included path like `~/.claude` is mounted whole or not at all — there is no way to hide part of it.

### File inclusions

Mount extra host paths into the sandbox:

```json
{
  "files": {
    "include": {
      "~/.npmrc": "~/.npmrc",
      "~/.gitconfig": "~/.gitconfig",
      "/opt/shared-data": "/data"
    }
  }
}
```

Keys are host paths, values are container paths. A missing host path is a warning and the entry is skipped. Two inclusions resolving to the *same* container path is an error — see [profiles](#profiles) for why that matters.

### Libraries

Mount sibling projects or dependencies, read-only by default:

```json
{
  "libraries": {
    "~/projects/shared-lib": "/libs/shared-lib",
    "~/projects/other-lib": { "path": "/libs/other-lib", "readwrite": true }
  }
}
```

Or ad-hoc:

```sh
hole start claude . --library ~/projects/shared-lib
hole start claude . --library ~/projects/other-lib:/libs/other:rw
```

If a library has its own `.hole/settings.json`, only its `files.exclude` entries are honored, scoped to that library's mount.

With [Docker-in-Docker](#docker-in-docker) on, libraries are also mounted into the sidecar at the same paths, so a container the agent starts can bind-mount one:

```sh
docker run -v /libs/shared-lib:/x alpine ls /x
```

A read-only library stays read-only there. Builds never needed this — `docker build` and `buildx` stream the context from the client — only a run-time bind mount does.

### Git worktrees

If your project is a git worktree, Hole mounts the related checkouts automatically — the main repository when you are in a linked worktree (a linked worktree's `.git` is only a pointer, so git would not work without it), and every linked worktree when you are in the main repository. Each is mounted at its own absolute path.

```json
{
  "git": {
    "worktreeLinks": "ro",
    "worktreePool": false
  }
}
```

`worktreeLinks` is `"ro"` (default), `"rw"`, or `"off"`. Explicit `libraries`/`--library` entries for the same path win. If `git` is not installed, this is skipped silently.

#### A place to create worktrees

The checkouts above are the ones that already existed when the sandbox started. A worktree the agent creates *during* a session lands in the container's writable layer and is lost at exit — while the admin entry it wrote into your repository survives, pointing at a directory that is no longer there.

`"worktreePool": true` fixes that. Started in the main repository, Hole creates a sibling directory of your project and mounts it read-write:

```
~/projects/myapp             # your project
~/projects/myapp-worktrees   # the pool
```

so `git worktree add ~/projects/myapp-worktrees/feature-x` inside the sandbox produces a checkout that is still valid on the host afterwards. Hole creates the directory and **never** deletes it — the checkouts in it are yours. Remove them with `git worktree remove` (or `git worktree prune`), not `rm -rf`, so the repository's admin files stay consistent.

The two mechanisms divide the work by location: a checkout inside the project comes with the project mount, one inside the pool with the pool mount, and any other one gets its own `worktreeLinks` mount. So `"worktreeLinks": "ro"` plus a pool means existing outside checkouts stay read-only while the pool — whose whole purpose is to be written to — is read-write.

The pool is only mounted when you start the sandbox **in the main repository**: from a linked worktree `git worktree add` cannot write the main repository's admin files anyway. `"worktreeLinks": "off"` switches the pool off too, and in a project settings file the pool needs your confirmation once, because it creates a host directory outside the project.

Tell your agent about it in the project's own instructions (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md` — Hole does not write these for you):

````markdown
## Git worktrees

If `$HOLE_WORKTREES_DIR` is set, create git worktrees inside it:

```sh
git worktree add "$HOLE_WORKTREES_DIR/<branch>" <branch>
```

Worktrees created anywhere else exist only inside the sandbox and are lost when it exits. If the
variable is not set, use the project's usual location.
````

Keep the `if` — that file is read by agents running on your host too, and inside a sandbox the variable is unset whenever the pool is not mounted (started in a linked worktree, `worktreeLinks: "off"`, or the pool disabled). Unconditional instructions would degrade into `git worktree add /<branch>` in every one of those cases.

**A worktree created mid-session gets no `files.exclude` over-mounts.** The mount set is fixed when the sandbox starts, so only checkouts that already existed then have their secrets hidden — each according to its own `.hole/settings.json`, exactly as a library does. This is the sharp edge of the mode's benefit: the sandbox cannot hide what did not exist yet.

### Network access

The sandbox has **no route to the internet** except through Hole's gateway, which denies everything by default — on every protocol and port. Allow what the project needs:

```json
{
  "network": {
    "allow": [
      "api.github.com",
      "*.npmjs.org",
      "db.example.com:5432",
      "github.com:22,443",
      "10.0.0.5:22,2222",
      "192.168.1.0/24:8080"
    ]
  }
}
```

Entry grammar: `<host>[:<port>[,<port>...]]`

| Host form | Matches |
|---|---|
| `example.com` | that exact name, nothing else |
| `*.example.com` | subdomains only — **not** the apex |
| `10.0.0.5` | one IPv4 address |
| `10.0.0.0/24` | an IPv4 range |

Ports default to `443,80` and apply to **TCP and UDP** alike. Wildcards are explicit: `example.com` never implies its subdomains.

How it works: the gateway is the sandbox's DNS server, router and firewall. A name you have not allowed does not resolve at all (a fast `NXDOMAIN` rather than a timeout), and an address is only reachable if the sandbox's own resolver handed it out for an allowed entry. So hardcoded IPs, third-party resolvers (`dig @8.8.8.8`) and DNS-over-TLS are all denied too.

Because filtering happens at the network layer, **no tool needs proxy configuration** — ssh, git over ssh, database clients, raw sockets and UDP all work as long as you allow the host and port.

Every **enabled** agent's own domains are always allowed, so the agent CLI works with empty settings — and because one image installs all of them, an agent that launches another agent works too. That does mean `hole start claude .` also permits codex's and gemini's endpoints; set [`container.enabledAgents`](#container-settings) to just the agent you use if you want a narrower surface — globally, so all your sandboxes share one narrower image, or in a project, which gives that project its own image. Each agent's list names the specific hosts its CLI needs, never a wildcard over a namespace shared with other tenants, and optional traffic such as usage telemetry is left out — allow it yourself if you want it.

Use `-n` to discover what a project needs: it writes every domain the sandbox resolved or was refused to `~/.hole/logs/{project}/network-access-{agent}-{id}.log`.

Known limitation: once an allowed name resolves to an address, that address stays reachable for the sandbox's lifetime, so an agent could in principle reach a *different* site sharing that address (common with CDNs).

The `-n` dump is a record of what the gateway's resolver was asked, not of every egress attempt. Two things are missing from it: direct-IP attempts blocked by the firewall, because they never produce a DNS query, and names resolved through the container's own fallback resolver — Docker's embedded `127.0.0.11`, which answers container names on the sandbox network without consulting the gateway. Neither is a way out of the sandbox (the firewall still decides what is reachable); they are gaps in the log, so treat the dump as "what the project asked to resolve", not as an audit trail.

### Host gateway domains

Let the sandbox reach services running on your host under a stable name:

```json
{
  "network": {
    "hostGatewayDomains": [
      "mydb.local:5432",
      "myapi.local:8080,8443"
    ]
  }
}
```

Each name resolves to the Docker host gateway. **The port list is required**: the firewall matches the host gateway *address*, not the name, so a port-less entry would expose every service on your machine — SSH, a TCP-exposed Docker socket, databases, anything bound to `0.0.0.0`. Several entries for the same name merge into one, opening the union of their ports.

For the same reason, the ports are unioned across *all* entries: with the example above the sandbox can reach the host gateway IP on 5432, 8080 and 8443, directly and without DNS. The names choose what resolves, not what the firewall permits. Don't use `localhost` or `127.0.0.1` — inside the container those are the container itself.

**Subdomains work the opposite way from `network.allow` above.** There is no `*.` syntax here — `"*.mydb.local:5432"` is rejected — but each entry claims the whole name *and everything under it*: with `"mydb.local:5432"` the names `db.mydb.local` and `a.b.mydb.local` resolve to the host gateway too. That is not extra reach (the firewall matches the address and the port union regardless of name), but it does mean **an entry whose name overlaps a real domain hijacks that entire domain for the sandbox**. `"example.com:8080"` makes `api.example.com` resolve to your host gateway even when `api.example.com` is in `network.allow`, so the real site becomes unreachable from the sandbox. Use names you control, ideally under a suffix that cannot resolve publicly (`.local`, `.internal`, `.test`).

### Subnet pool

Each sandbox takes two `/24` networks from Hole's own pool. Change it if the default collides with your VPN or LAN:

```json
{
  "network": {
    "subnetPool": "10.99.0.0/16"
  }
}
```

Must be a `/23` or larger (each sandbox needs two `/24`s). A `/16` supports ~127 concurrent sandboxes.

### Dependencies

Extra apt packages baked into the sandbox image:

```json
{
  "dependencies": ["make", "openjdk-17-jdk", "postgresql-client=15+248"]
}
```

Installed during the image build, which uses your host's network, so the Ubuntu repositories do **not** need to be in `network.allow`.

### Container settings

```json
{
  "container": {
    "memoryLimit": "8g",
    "memorySwapLimit": "8g",
    "baseImage": "ubuntu:24.04",
    "docker": true,
    "enabledAgents": ["claude", "codex"]
  }
}
```

- `memoryLimit` / `memorySwapLimit` — limits for the agent container
- `baseImage` — must stay Ubuntu 24.04-based (the image build uses apt)
- `docker` — enable the [Docker-in-Docker](#docker-in-docker) sidecar
- `enabledAgents` — which agents get installed into the image (default: all registered agents, including your custom ones). The agent you start must be in this list.

The container user mirrors your host user — same username and home path — so agent config paths look identical inside and outside the sandbox. The user has passwordless `sudo`; the container is the boundary, not the user.

**Image sharing:** projects whose settings do not change the image content share one image, so a rebuild in one project benefits all of them. A project that adds a dependency, changes the base image, changes `enabledAgents`, or has its own setup hook gets its own image — and the start banner tells you which, and why.

### Docker-in-Docker

Give the agent its own Docker daemon so it can run `docker` and `docker compose` (for example to spin up PostgreSQL or Redis for tests):

```json
{
  "container": { "docker": true }
}
```

Or ad-hoc:

```sh
hole start claude . --with-docker
```

A `docker:dind-rootless` sidecar starts on the internal sandbox network; the agent gets the Docker CLI with the compose and buildx plugins automatically, so `docker build`, `docker buildx build` and `docker compose` all work inside the sandbox. The sidecar image is pinned to an exact digest rather than a moving tag, so the daemon version inside your sandboxes changes only when a Hole release bumps it — never silently under a version you already have.

- **Accessing services**: containers started inside DinD are reachable from the agent at hostname `docker`, not `localhost`. Bind ports to all interfaces (`3307:3306`, not `127.0.0.1:3307:3306`).
- **Workspace bind mounts**: the project is mounted at the same absolute path in both containers, so bind mounts in your compose files resolve correctly.
- **File exclusions** are mirrored onto the sidecar, so a container started inside the sandbox cannot bind-mount a path the agent was meant not to see.
- **Libraries** are mirrored too, at the same paths, so `docker run -v /libs/shared:/x` and compose `volumes:` entries pointing at a library resolve. A `:ro` library stays read-only inside the nested container: the daemon is rootless, and the kernel refuses to lift the read-only flag off a mount inherited into its user namespace. That guarantee is defense-in-depth, not a hard boundary — an escape from the privileged sidecar container lands outside that namespace, where the remount works again.
- **`files.include` targets are not** mounted into the sidecar. They stay available to the agent as always; a single file like `~/.npmrc` has no plausible use as a nested bind mount, so there is no reason to widen what the sidecar can see. If you need one there, move the entry to `libraries`. Builds are unaffected either way — `docker build` and `buildx` stream the context from the client, so they work with paths the daemon cannot see.
- **Docker Hub must be allowed explicitly**: `network.allow` has to contain `"docker.io"` or `"*.docker.io"`, or the sandbox cannot pull from Hub at all. No other spelling counts — not `index.docker.io`, not `registry-1.docker.io`. Allowing it is a real decision, not a formality: Hub is a platform anyone can publish to, so the whole of it becomes reachable, and the cache reaches it over a channel the gateway does not filter.
  ```json
  { "container": { "docker": true }, "network": { "allow": ["docker.io"] } }
  ```
  Unrestricted mode (`-u`) already allows every host, so it needs no entry.
- **Image cache**: with Hub allowed, Hole attaches a long-lived pull-through cache (`hole-registry`) to the sandbox so repeated pulls do not re-download. It caches **Docker Hub only**. Since the cache is the only Hub path `"docker.io"` opens, a cache that fails to start means no Hub pulls. Hole waits for it to actually come up before the daemon is pointed at it: a cache that exits during startup (its upstream unreachable, most often) is reported with the reason and removed instead of left restarting in the background, and the sandbox starts without one. When the last sandbox exits, the cache container is stopped — never removed — so nothing of Hole's keeps running once `hole list` is empty, and the next start brings the same cached blobs back. To also allow direct pulls through the gateway, allow the endpoints themselves — `["*.docker.io", "*.cloudflare.docker.com"]` covers the registry, auth and the CDN blobs redirect to; if a pull is still denied, run with `-n` and the dump names the host to add.
- **Non-Hub registries**: not cached, and governed exactly like any other host — allow their domains, e.g. `"network": { "allow": ["ghcr.io", "*.githubusercontent.com"] }`.
- **Trust**: a *project's* settings file asking for `container.docker` needs your confirmation, since it adds a privileged container — see [project trust](#project-trust). `--with-docker` and your global settings are your own choice and never prompt.
- **Security**: the daemon runs **rootless** — as an unprivileged user in a user namespace — so a container the agent starts through it, even a `--privileged` one, cannot read the host's disks or files. It has no internet route of its own either; its traffic is filtered by the same gateway. The sidecar *container* is still privileged, because Docker-in-Docker requires it, so this is defense-in-depth rather than a hard boundary: a kernel or container-runtime escape from inside the sidecar could still reach the host. Keep your host's kernel and container runtime patched, and treat "the agent can run Docker" as a larger surface than the rest of the sandbox. If you do not need a real daemon, prefer leaving DinD off.

### Environment variables

```json
{
  "environment": {
    "MY_VAR": "value",
    "NODE_ENV": "development",
    "PROJECT_PATH": "$PROJECT_PATH",
    "CACHE_DIR": "$HOME/.cache/agent"
  }
}
```

Passed to the agent container and, when enabled, to the DinD sidecar.

`$VAR` and `${VAR}` are resolved from **your** environment when the sandbox starts, so a value can
carry a host variable into the sandbox. `$HOME` is the exception: it becomes the sandbox home, since
that is what a container-side value means. An undefined variable is left as written and warned
about, rather than silently becoming empty.

### Agent arguments

Default CLI arguments for an agent:

```json
{
  "agents": {
    "claude": { "args": ["--model", "opus"] }
  }
}
```

Applied only when that agent starts. Arguments you pass after `--` come last, so an ad-hoc flag overrides one from settings. With `-d` they are simply unused.

Configured arguments expand `$VAR` the same way `environment` values do. Arguments given on the command line do not — your shell already had its turn at those, and expanding again would defeat the quoting you used to keep a literal `$`.

> A project's `.hole/settings.json` can therefore inject agent flags. Those flags act inside the sandbox, so they are not gated — the settings that reach your host or widen what is mounted need your confirmation instead, see [project trust](#project-trust).

### Hooks

| Hook | Runs where | When | On failure |
|---|---|---|---|
| `hooks.setupHost` | host | before any Docker work | aborts startup (`cleanupHost` still runs) |
| `hooks.setup` | container, during the image build | when the image is built | aborts the build |
| `hooks.prestart` | container | every start, before the agent CLI | aborts startup |
| `hooks.cleanupHost` | host | during teardown | logged, teardown continues |

```json
{
  "hooks": {
    "setupHost": [{ "script": ".hole/setup-host.sh" }],
    "setup": [{ "script": ".hole/setup.d/*.sh" }],
    "prestart": [{ "script": ".hole/prestart.sh" }],
    "cleanupHost": [{ "script": ".hole/cleanup-host.sh" }]
  }
}
```

Every entry is a literal path or a glob. Matches run in lexicographic order, so `001-`, `002-` prefixes give a predictable sequence. A missing script or a pattern matching nothing is a warning, not an error.

Hooks receive `HOLE_PROJECT_DIR`, `HOLE_PROJECT_NAME`, `HOLE_INSTANCE_NAME`, `HOLE_INSTANCE_ID` and `HOLE_SANDBOX_NETWORK` in their environment.

`cleanupHost` additionally receives `HOLE_IS_LAST_INSTANCE`, `true` when the sandbox being torn down is the only one left. Use it to release host-side infrastructure that all your sandboxes share — a proxy, a tunnel, a database container — without stopping it out from under a sandbox still running in another terminal:

```bash
#!/usr/bin/env bash
# Stop the shared docker proxy, but only once no other sandbox is still using it.

if [[ "${HOLE_IS_LAST_INSTANCE}" == "true" ]]; then
  bash ~/.hole/docker-proxy.sh stop
fi
```

Sandboxes that Hole has already given up on (both their CLI and their watchdog are gone, so the next garbage collection will reclaim them) do not count — otherwise one crashed sandbox would keep your shared resource alive indefinitely. Two sandboxes exiting at the very same moment each still see the other, so neither is told it is last; the resource stays up until the next single exit, which is the harmless way for this to be wrong.

`setupHost`, `cleanupHost` and `setup` in a *project's* settings file need your confirmation before they run — see [project trust](#project-trust).

`hooks.setup` scripts are part of the image identity, so editing one rebuilds the image.

> `cleanupHost` scripts normally run in Hole's detached teardown supervisor, which has **no TTY**. Their output goes to the run log; interactive scripts are not supported there.

### Profiles

Named overlays for different modes of work, selected at start time:

```json
{
  "network": { "allow": ["api.github.com"] },
  "profiles": {
    "research": {
      "network": { "allow": ["*.wikipedia.org"] }
    },
    "docker": {
      "container": { "docker": true },
      "dependencies": ["make"]
    },
    "research-docker": {
      "extends": ["research", "docker"],
      "agents": { "claude": { "args": ["--model", "opus"] } }
    }
  }
}
```

```sh
hole start claude:research .
hole start claude:research-docker .
```

A profile accepts exactly the same settings as the root (except `profiles` itself), can be defined in either settings file, and can `extends` other profiles — including across files, so a project profile may extend a global one. Names must match `^[a-z0-9][a-z0-9-]*$`.

Merge order: global base → global overlays (parents first) → project base → project overlays. So the project still overrides the global file, and the selected profile is the last word within each.

A profile you ask for that no settings file defines is an **error**, not a silent no-op — Hole lists what each file does define. Profiles only work with `start`.

**Profiles only add; they never take away.** This keeps effective permissions readable, and has two consequences:

1. **Keep the base minimal.** A broad base cannot be narrowed by a profile, so put in the base only what every mode needs and let each profile add the rest.
2. **A mount whose source varies between profiles belongs in each profile**, not in the base. `files.include` is keyed by *host* path, so a base `~/.claude → ~/.claude` and a profile `~/claude-review → ~/.claude` would both survive the merge and target the same container path — which Hole rejects, naming both sources.

### Configuration examples

#### Run Maven builds and tests

_These can go in the global `~/.hole/settings.json` or a project's `.hole/settings.json`._

**1. Create Maven settings for the agent.** You probably don't want to hand `~/.m2/settings.xml` and its secrets to the agent, so make a separate `~/.m2/agent-settings.xml`. No proxy configuration is needed — the sandbox filters at the network layer, so Maven just works.

If you need toolchains, note the JDK path inside the sandbox depends on the architecture:

```
/usr/lib/jvm/java-17-openjdk-amd64   # x86 (standard Linux, WSL)
/usr/lib/jvm/java-17-openjdk-arm64   # ARM (Apple Silicon)
```

```xml
<!-- ~/.m2/agent-toolchains.xml -->
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

**2. Mount them and allow the repositories:**

```json
{
  "dependencies": ["openjdk-17-jdk", "maven"],
  "files": {
    "include": {
      "~/.m2/repository": "~/.m2/repository",
      "~/.m2/agent-settings.xml": "~/.m2/settings.xml",
      "~/.m2/agent-toolchains.xml": "~/.m2/toolchains.xml"
    }
  },
  "network": {
    "allow": ["repo.maven.apache.org", "*.apache.org", "nexus.mycompany.internal"]
  }
}
```

#### Node project with a private registry

```json
{
  "dependencies": ["nodejs", "npm"],
  "files": {
    "include": { "~/.npmrc": "~/.npmrc" },
    "exclude": [".env", ".env.*", "node_modules"]
  },
  "network": {
    "allow": ["registry.npmjs.org", "*.npmjs.org", "npm.mycompany.internal"]
  }
}
```

#### Reaching a database on your host

```json
{
  "network": {
    "hostGatewayDomains": ["mydb.local:5432"]
  }
}
```

Then connect to `mydb.local:5432` from inside the sandbox.

## Logs

| Path | Contents |
|---|---|
| `~/.hole/logs/run-<date>-<agent>-<pid>.log` | per-run debug log: every runtime command, timings, teardown detail (kept ~7 days) |
| `~/.hole/logs/<project>/network-access-<agent>-<id>.log` | the `-n` dump of resolved and refused domains |
| `~/.hole/instances/` | one file per running sandbox, powering `hole list` |
| `~/.hole/trust.json` | which projects' settings you accepted, see [project trust](#project-trust) |

Run with `-d` to see debug output on the console as well.
