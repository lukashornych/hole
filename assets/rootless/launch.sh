#!/bin/sh
set -eu
# dockerd invokes iptables/nft/ip by name. Never inherit the caller's PATH.
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
fail() { echo "hole rootless: $*" >&2; exit 1; }
uid=$(id -u)
[ "$uid" -ne 0 ] || fail 'requires a non-root user'
for tool in rootlesskit slirp4netns newuidmap newgidmap dockerd docker-init unshare nsenter ip iptables sed awk; do
    command -v "$tool" >/dev/null || fail "missing prerequisite: $tool"
done
for file in /etc/subuid /etc/subgid; do
    awk -F: -v user="$(id -un)" -v uid="$uid" '($1==user || $1==uid) && $3>=65536 {ok=1} END {exit !ok}' "$file" ||
        fail "missing subordinate ID range (at least 65536 IDs) in $file"
done
# An unprivileged container does not receive TUN by default. Check both the
# node and an actual open, so device-cgroup denials produce a useful error too.
[ -c /dev/net/tun ] || fail 'missing prerequisite: /dev/net/tun character device (required for slirp4netns TAP networking)'
( exec 9<>/dev/net/tun ) 2>/dev/null || fail 'cannot open /dev/net/tun: check device permissions and runtime device access policy'
unshare -Ur true || fail 'unprivileged user namespaces are not enabled (check user.max_user_namespaces, kernel.unprivileged_userns_clone, seccomp and AppArmor)'
export XDG_RUNTIME_DIR=/run/user/$uid
mkdir -p "$XDG_RUNTIME_DIR"
[ -w "$HOME/.local/share/docker" ] || fail 'data-root is not writable by the non-root user'
# Test the actual mappings, not just the presence of setuid helpers. no-new-privileges
# and nosuid filesystems can make otherwise correctly installed helpers unusable.
rootlesskit --net=host true || fail 'user namespace UID/GID mapping failed: check subuid/subgid, setuid newuidmap/newgidmap, nosuid and no-new-privileges'
if [ "${1:-}" = --check ]; then exit 0; fi
# Resolve only the runtime-injected IPv4 address, not DNS or a caller value.
host_ip=$(awk '$1 !~ /:/ { for(i=2;i<=NF;i++) if($i=="host.internal") {print $1;exit} }' /etc/hosts)
# A runtime may supply a nonzero address in 0/8 (e.g. a virtual host gateway).
# Reject the unspecified address itself, not the entire first-octet-zero range.
printf '%s\n' "$host_ip" | awk -F. 'NF!=4 {exit 1} {for(i=1;i<=4;i++) if($i !~ /^[0-9]+$/ || $i>255) exit 1; if($1==127 || ($1==0 && $2==0 && $3==0 && $4==0)) exit 1}' || fail 'no usable IPv4 host-gateway address supplied by the container runtime'
umask 077
sed "s/HOLE_HOST_GATEWAY_IP/$host_ip/g" /etc/hole/daemon.json > "$XDG_RUNTIME_DIR/daemon.json"
dockerd --validate --config-file="$XDG_RUNTIME_DIR/daemon.json" || fail 'invalid generated dockerd configuration'
if [ "${HOLE_REQUIRE_CGROUP:-0}" = 1 ]; then
    [ -f /sys/fs/cgroup/cgroup.controllers ] && [ -w /sys/fs/cgroup/cgroup.procs ] ||
        fail 'resource limits require cgroup-v2 user delegation (a writable delegated cgroup, not a host cgroup mount)'
fi
# A fresh outer netns is created by the container runtime. Only the short-lived
# route helper can change it. Do not start slirp until its route is in place.
i=0
until ip -4 route show default | grep -Fq "default via $HOLE_GATEWAY_IP "; do
    i=$((i+1)); [ "$i" -lt 60 ] || fail 'gateway route helper did not configure the sidecar network namespace'
    sleep 1
done
exec rootlesskit --state-dir="$XDG_RUNTIME_DIR/rootlesskit" \
    --net=slirp4netns --mtu=1500 --disable-host-loopback --port-driver=builtin \
    --copy-up=/etc --copy-up=/run --propagation=rslave -p 0.0.0.0:2375:2375/tcp \
    docker-init -- /usr/local/bin/daemon.sh
