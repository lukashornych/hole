ARG BASE_IMAGE=ubuntu:24.04

FROM ${BASE_IMAGE}

LABEL com.hole.image=gateway

# Use bash with pipefail so any failed command in a pipe aborts the build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

EXPOSE 53/udp
EXPOSE 53/tcp

ARG COREDNS_VERSION=1.12.1

# The base image is Ubuntu rather than Alpine for one hard reason: Hole's filtering depends
# on dnsmasq's --nftset option, and Alpine's dnsmasq package is compiled with `no-nftset`
# (verified via `dnsmasq --version`), so the gateway could not enforce anything there.
# Debian/Ubuntu builds enable it; the entrypoint re-checks at startup and fails loudly if a
# future package drops the feature.
#
# Roles in this one container (they must share a network namespace — nftset writes go to the
# kernel of the namespace dnsmasq runs in):
#   CoreDNS  :53   DNS policy: answers only allowed names, NXDOMAIN for the rest
#   dnsmasq  :5353 resolves approved names and records answers in nftables sets
#   nftables       forward-chain filtering and masquerading
#
# The final `command -v dig` is a build-time assertion, not a probe: the compose healthcheck
# resolves through dig, so an image without it could never report healthy and no sandbox could
# start. Failing the build is the loud version of that.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      dnsmasq-base \
      iproute2 \
      nftables \
      bind9-dnsutils \
    && ARCH="$(dpkg --print-architecture)" \
    && case "${ARCH}" in \
         amd64) GOARCH=amd64 ;; \
         arm64) GOARCH=arm64 ;; \
         *)     echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/coredns/coredns/releases/download/v${COREDNS_VERSION}/coredns_${COREDNS_VERSION}_linux_${GOARCH}.tgz" \
       | tar -xz -C /usr/local/bin \
    && chmod +x /usr/local/bin/coredns \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && command -v dig >/dev/null

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
