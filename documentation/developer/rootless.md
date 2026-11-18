# Unprivileged Docker sidecar

Opt in per project, globally, or in a profile:

```json
{
  "container": {
    "docker": true,
    "dockerRootless": true
  }
}
```

`container.dockerRootless` defaults to false. `--with-docker` may replace
`container.docker: true`; the rootless option alone does not enable a sidecar.
The boolean `container.docker` remains compatible with existing settings.

This checkout's default already runs a rootless daemon, but its outer sidecar
starts as root and is privileged. The new option removes that outer privilege.
It does not make the outer Docker engine, the agent's route setup, or the gateway
rootless. Hole still requires access to a container engine. No host distribution,
VM product, service manager, or orchestrator is assumed.

## Architecture and privileges

`internal/sandbox/rootless.go` changes only the opt-in Compose services. The
sidecar image derives from the same digest-pinned DinD image as the default path.
Its assets have a separate content hash, and its UID/GID match the agent's build
identity. No host bind mount is chowned. The sidecar starts as that non-root user;
its capability bounding set contains only SETUID/SETGID for the mapping helpers.
It has no NET_ADMIN, SYS_ADMIN, host networking, host PID namespace or host socket.
The only passed device is `/dev/net/tun`, with read/write access, for slirp4netns
TAP creation. This does not grant network administration in the outer namespace;
the TAP interface is created inside rootlesskit's child namespace.

Nested namespace creation and mounts need the sidecar's seccomp and AppArmor
restrictions relaxed, and its system-path masks removed. The opt-in Compose
service explicitly requests `seccomp=unconfined`, `apparmor=unconfined` and
`systempaths=unconfined`. It does **not** request `privileged`. A host security
policy that disallows these operations will reject startup. For background, see
[Docker's rootless prerequisites](https://docs.docker.com/engine/security/rootless/)
and [BuildKit's explanation of these runtime options](https://github.com/moby/buildkit/blob/master/docs/rootless.md).

The runtime creates an isolated outer sidecar network namespace, attached only
to Hole's internal sandbox network. A one-shot `docker-route` service receives
NET_ADMIN **only in that namespace**, sets the default route through Hole's
gateway, and exits. It shares no filesystem or PID namespace with the sidecar.
The launcher waits for this route and fails closed if it does not appear.

The launcher then execs:

```text
rootlesskit --net=slirp4netns --mtu=1500 --disable-host-loopback
  --port-driver=builtin --copy-up=/etc --copy-up=/run --propagation=rslave
  -p 0.0.0.0:2375:2375/tcp docker-init -- daemon.sh
```

`daemon.sh` validates the generated config and execs `dockerd`. It uses a fixed
system PATH for iptables, nft and ip helpers. Project `environment` settings are
not passed to the daemon, so they cannot change its launcher, PATH or config.
The generated template is mounted read-only. Only the runtime-provided IPv4
host-gateway address is substituted; `dockerd --validate` runs before launch.
The daemon has explicit user-writable data-root, exec-root and pidfile paths,
explicit Unix/TCP listeners, `rootless: true`, and `live-restore: false`.

The API is forwarded only to the sidecar's internal address, not published to the
host. As with the existing mode, sandbox peers have full access to this daemon.
`--disable-host-loopback` blocks slirp's alias to the launcher's loopback.

The storage driver is deliberately `vfs`, with the containerd image store
disabled: it does not require /dev/fuse, host overlay support or extra capabilities.
This costs disk space and build performance compared with overlayfs.

Unlike the separate cohost reference implementation, this sidecar retains Docker's
default bridge. Plain `docker run` needs it. It lives in the **nested dockerd
namespace** and does not create another route around Hole's gateway.

## Prerequisites and errors

Checks run inside the actual sidecar, where host/container restrictions apply:

- A non-root UID/GID; at least 65,536 subordinate IDs in both `/etc/subuid` and
  `/etc/subgid`. The image provisions ranges above the mapped UID/GID, with no
  overlap with that user. These are **sidecar** mappings, not changes to the
  developer machine's account files.
- `newuidmap`, `newgidmap`, rootlesskit and slirp4netns from the pinned image.
  The launcher tests an actual rootlesskit UID/GID mapping. Missing binaries,
  ineffective setuid/file capabilities, `nosuid`, or `no-new-privileges` produce
  a named prerequisite error.
- A working `/dev/net/tun` character device in the outer engine's Linux host
  (the runtime's Linux VM on macOS). Compose passes it to the sidecar explicitly.
  The launcher checks that the node exists and can actually be opened. Missing
  host devices can also be rejected by Docker before the launcher runs. Hole
  does not create a device on macOS or change the host's device permissions.
- Unprivileged user namespaces enabled by the outer runtime's Linux kernel and
  security policy. The `unshare -Ur` probe names userns sysctls, seccomp and
  AppArmor in its error. Hole never changes host-wide sysctls automatically.
- Writable daemon state and a usable IPv4 host-gateway address.
- For configured resource limits, cgroup v2 **user delegation** and a working
  systemd cgroup driver. When `container.memoryLimit` or `memorySwapLimit` is
  supplied, this mode conservatively requires both a writable delegated cgroup
  and a daemon reporting `2 systemd`; otherwise startup fails explicitly rather
  than suggesting that nested limits work. A typical container's read-only
  cgroup mount does not meet this requirement. Hole does not mount the host's
  entire cgroup hierarchy or provision a user service manager. Use a host/runtime
  with delegation supplied to the sidecar, or omit limits in this profile.

The last check is intentionally strict. The existing memory settings still apply
to the agent; they do not create or delegate a nested daemon cgroup. This version
does not automate provisioning for nested per-container limits. With no configured
limits the daemon may report cgroup driver `none`; do not rely on nested
`docker run --memory/--cpus` in that case.

Any missing prerequisite aborts startup before the agent and prestart hooks run.
Sidecar logs and health-probe output are included in startup diagnostics. There
is no automatic fallback to a privileged sidecar.

## Networking and the daemon namespace

Egress follows this path:

```text
nested container → dockerd bridge → slirp4netns → outer sidecar route
                 → Hole gateway DNS + L3/L4 firewall → allowed destination
```

`network.allow`, runtime `hole modify` rules, and `network.hostGatewayDomains`
continue to use the existing gateway. Nested container DNS is explicitly set to
that gateway. `host-gateway-ips` uses the outer engine's injected host-gateway
address, so nested `--add-host=name:host-gateway` and the configured DNS zones
agree. Host access remains limited to configured ports by the gateway firewall.
No new host relay or unfiltered external network is attached to the sidecar.

**Never enter PID 1's network namespace to operate on nested Docker bridges.**
PID 1 is the rootlesskit launcher. `br-<network-id>` and container veths live in the
namespace of its descendant whose `/proc/<pid>/comm` is `dockerd`.

Use the outer Docker engine while the sandbox is running:

```sh
docker exec "$sidecar" hole-dockerd-netns
# pid=42 netns=net:[4026533000] path=/proc/42/ns/net

docker exec "$sidecar" hole-dockerd-netns ip link show
docker exec "$sidecar" hole-dockerd-netns iptables -S
```

The helper resolves the actual dockerd process, refuses missing/ambiguous results
or a namespace equal to PID 1's, and opens namespace FDs before invoking `nsenter`.
It enters the owning user namespace and the network namespace. The reported PID
is **sidecar-local**, not a PID on the developer machine or daemon host. Do not
cache it across restarts. Call the helper again for each operation; keep the FD
open if implementing a longer-lived host-side operation. Do not use
`docker inspect .State.Pid` as though it were dockerd's PID.

Hole's existing bridge-netfilter repair still targets the **outer engine's**
sandbox bridge. It must not be redirected into the nested sidecar: that is a
different bridge owned by a different daemon. The sidecar's namespace helper is
for operations on networks created through the sandbox's `DOCKER_HOST`.

## Mounts and hooks

Project mounts, library RO/RW flags, included Docker files, exclusion masks and
`git.worktreeLinks` use the same mount builder as the default path. The sidecar's
outer UID/GID match the agent, so root (UID 0) in a nested container maps to that
user and can write its RW host mounts. Other nested UIDs map into the subordinate
range: a nested `--user 1000` is **not** the developer's host UID. Use nested UID 0
for developer-owned bind mounts, or arrange appropriate subordinate-ID ownership
outside Hole. Hole never recursively changes host ownership to make this work.

The agent image already ships Docker CLI, buildx and Compose. Build contexts are
streamed over the API; they do not require extra sidecar mounts.

Hook timing and identity are unchanged:

- `setup` runs as the agent user during its image build; it has no live sidecar.
- `setupHost` runs as the CLI's host user, before sidecar creation. In opt-in mode
  it receives `HOLE_DOCKER_ROOTLESS=1` and the future `HOLE_DOCKER_CONTAINER` name,
  but cannot resolve a live dockerd PID yet.
- `prestart` runs as the agent user after gateway, route helper and daemon health
  checks succeed. It receives those same variables plus the existing
  `DOCKER_HOST=tcp://docker:2375`. Docker API operations work normally. Its `ip`,
  `iptables`, or `sudo` commands affect the agent namespace, **not dockerd's**.
  Host-side network operations must use the outer engine/helper shown above;
  the outer Docker socket is intentionally not mounted into the agent.
- `cleanupHost` runs after Compose teardown; the daemon namespace is already
  gone. Namespace-specific cleanup must not depend on this hook entering it.

No persistent namespace bind mounts or external veths are created. Compose owns
both new services; the existing teardown/watchdog/GC removes them and the daemon
volume. Nested containers and their bridges die with the sidecar, even when a
nested workload was deliberately left running.

## Verification

```sh
go test ./...
go test -tags=e2e,rootless ./test/e2e -run '^TestRootlessAcceptance$' -count=1 -timeout=25m
```

The e2e test requires a working Docker engine and Compose, a non-root test user,
user namespaces allowed by its runtime, and test directories accessible to that
engine. It builds the real `hole` binary and runs `hole start` with a test agent;
no paid agent API or login is needed. It fails rather than skips on a rootless
prerequisite error. Network access is needed to build images and pull BusyBox.

It checks rootless info/non-root sidecar/no privilege, plain run, BuildKit RUN,
buildx, Compose, an internal network, a reachable host-gateway target, a known
listening but forbidden port by literal IP, slirp loopback denial, RW libraries,
linked worktrees, hooks, and actual bridge visibility via the dockerd netns helper.
It leaves a nested workload running, then checks outer container/network/volume
cleanup. For kernel-level leak auditing on the daemon host, additionally compare
`lsns -t net` and `ip link` before/after the test; the host may be a remote Linux
machine, so the portable test does not pretend the CLI machine can inspect it.

The existing Compose golden tests are not regenerated. They pin the opt-out
service definitions byte-for-byte; another test toggles the new option on and
back off and compares the generated YAML. Schema changes still participate in
Hole's normal embedded-asset version hashing, as other schema changes do.
