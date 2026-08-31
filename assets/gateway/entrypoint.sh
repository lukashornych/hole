#!/bin/bash
set -euo pipefail

# The container starts with net.ipv4.ip_forward=1 already set (compose `sysctls`; /proc/sys is
# read-only in an unprivileged container, so it cannot be enabled from here instead), which makes
# this container a router before anything below has run. Drop forwarded traffic first, so the
# window between container start and the real ruleset is closed — and so an abort in any check
# below leaves the sandbox with no route rather than an unfiltered one.
#
# The real ruleset re-creates this table in a single nft transaction, so replacing it opens no gap.
nft -f - <<'EOF'
table inet hole
delete table inet hole
table inet hole {
    chain forward {
        type filter hook forward priority filter; policy drop;
    }
}
EOF

# Runtime-mounted, generated per run by internal/network.
COREFILE_TEMPLATE=/etc/hole/Corefile
DNSMASQ_CONF=/etc/hole/dnsmasq.conf
NFT_TEMPLATE=/etc/hole/nftables.rules

# The sandbox subnet is passed in so interfaces can be identified by address instead of by
# name — compose does not guarantee eth0/eth1 ordering across the two networks.
: "${HOLE_SANDBOX_SUBNET:?HOLE_SANDBOX_SUBNET must be set}"

# Filtering depends on dnsmasq recording answered addresses in nftables sets. Without that
# option the sandbox would resolve names it could never connect to, so refuse to start.
# The output is captured first rather than piped into grep: under `pipefail`, grep -q exiting
# on the first match can leave dnsmasq killed by SIGPIPE, which would fail this check against
# a build that does support nftset.
dnsmasq_version="$(dnsmasq --version 2>&1)"
if ! grep -q '[^-]nftset' <<<"${dnsmasq_version}"; then
  echo "ERROR: this dnsmasq build has no nftset support; the gateway cannot enforce network rules" >&2
  head -2 <<<"${dnsmasq_version}" >&2
  exit 1
fi

# The address the runtime injects for `host-gateway` lives in /etc/hosts, and it is read from there
# rather than through getent: a runtime that injects both an IPv4 and an IPv6 entry (OrbStack) makes
# glibc answer with the IPv6 one, and an IPv4-only filter is then left with nothing.
# `host.containers.internal` is Podman's built-in name for the same address.
host_gateway_ip="$(awk '$1 !~ /:/ {
    for (i = 2; i <= NF; i++)
      if ($i == "host.internal" || $i == "host.containers.internal") { print $1; exit }
  }' /etc/hosts)"

# A host gateway zone exists exactly when the generated Corefile carries the placeholder, so the
# entrypoint needs no extra signal from compose — and cannot drift out of sync with the policy.
if grep -q '{HOST_GATEWAY_IP}' "${COREFILE_TEMPLATE}"; then
  case "${host_gateway_ip}" in
    ""|127.*|0.0.0.0)
      echo "ERROR: no usable IPv4 host gateway address in /etc/hosts (found '${host_gateway_ip:-none}');" >&2
      echo "       network.hostGatewayDomains cannot work on this container runtime" >&2
      exit 1
      ;;
  esac
elif [[ -z "${host_gateway_ip}" ]]; then
  # Never leave the placeholder in the rules: an unresolved host gateway must not turn into
  # a syntactically valid rule pointing somewhere unintended.
  host_gateway_ip="127.0.0.1"
fi

subnet_prefix="$(echo "${HOLE_SANDBOX_SUBNET}" | cut -d/ -f1 | cut -d. -f1-3)"

sandbox_if=""
internet_if=""
while read -r iface addr; do
  [[ "${iface}" == "lo" ]] && continue
  if [[ "$(echo "${addr}" | cut -d. -f1-3)" == "${subnet_prefix}" ]]; then
    sandbox_if="${iface}"
  else
    internet_if="${iface}"
  fi
done < <(ip -o -4 addr show | awk '{split($4, a, "/"); print $2, a[1]}')

if [[ -z "${sandbox_if}" || -z "${internet_if}" ]]; then
  echo "ERROR: could not identify sandbox (${sandbox_if:-none}) and internet (${internet_if:-none}) interfaces" >&2
  ip -o -4 addr show >&2
  exit 1
fi

echo "Gateway interfaces: sandbox=${sandbox_if} internet=${internet_if}, host gateway=${host_gateway_ip}"

mkdir -p /tmp/hole
sed -e "s/{HOST_GATEWAY_IP}/${host_gateway_ip}/g" "${COREFILE_TEMPLATE}" > /tmp/hole/Corefile
sed -e "s/{HOST_GATEWAY_IP}/${host_gateway_ip}/g" \
    -e "s/{SANDBOX_IF}/${sandbox_if}/g" \
    -e "s/{INTERNET_IF}/${internet_if}/g" \
    "${NFT_TEMPLATE}" > /tmp/hole/nftables.rules

nft -f /tmp/hole/nftables.rules

# dnsmasq resolves only what CoreDNS already approved and records the answers in nftables sets.
dnsmasq --conf-file="${DNSMASQ_CONF}" --keep-in-foreground &
dnsmasq_pid=$!

trap 'kill "${dnsmasq_pid}" 2>/dev/null || true' TERM INT

exec coredns -conf /tmp/hole/Corefile
