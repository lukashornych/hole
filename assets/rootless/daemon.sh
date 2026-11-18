#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export DOCKERD_ROOTLESS_ROOTLESSKIT_STATE_DIR="$XDG_RUNTIME_DIR/rootlesskit"
# copy-up makes /run private. Remove only inherited helper symlinks there so
# dockerd and iptables can create their own runtime files in this user namespace.
for file in /run/docker /run/containerd /run/xtables.lock; do
    if [ -L "$file" ]; then rm "$file"; fi
done
dockerd --validate --config-file="$XDG_RUNTIME_DIR/daemon.json"
exec dockerd --config-file="$XDG_RUNTIME_DIR/daemon.json"
