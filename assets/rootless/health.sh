#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# The healthcheck runs in the outer netns, exercising the forwarded API port.
info=$(docker -H tcp://127.0.0.1:2375 info --format '{{json .SecurityOptions}}')
printf '%s\n' "$info" | grep -q 'name=rootless' || { echo 'hole rootless: daemon did not report rootless mode'; exit 1; }
hole-dockerd-netns
if [ "${HOLE_REQUIRE_CGROUP:-0}" = 1 ]; then
    cg=$(docker -H tcp://127.0.0.1:2375 info --format '{{.CgroupVersion}} {{.CgroupDriver}}')
    [ "$cg" = '2 systemd' ] || { echo 'hole rootless: limits require cgroup-v2 user delegation and the systemd cgroup driver'; exit 1; }
fi
