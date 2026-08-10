ARG BASE_IMAGE=ubuntu:24.04

FROM ${BASE_IMAGE}

LABEL com.hole.image=gateway

# Use bash with pipefail so any failed command in a pipe aborts the build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

EXPOSE 53/udp
EXPOSE 53/tcp

ARG COREDNS_VERSION=1.12.1

# sha256 of the official release tarballs, as published next to them
# (coredns_<version>_linux_<arch>.tgz.sha256). CoreDNS enforces the whole DNS policy, so it is
# not installed unverified — the tarball arrives over the network from a GitHub redirect chain,
# and these are the only reason to trust what comes back. They MUST be refreshed together with
# COREDNS_VERSION; a mismatch fails the build, and so does a version bump that forgets them.
ARG COREDNS_SHA256_AMD64=886206d686363cd7d71c62ac47287692b954da3dfc9bcbffedd80d4b69358001
ARG COREDNS_SHA256_ARM64=f1c9030a7fbf3c9f5421894211b3bee62de7e77d260fc4282a0d8104376b6300

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
         amd64) GOARCH=amd64; COREDNS_SHA256="${COREDNS_SHA256_AMD64}" ;; \
         arm64) GOARCH=arm64; COREDNS_SHA256="${COREDNS_SHA256_ARM64}" ;; \
         *)     echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/coredns.tgz \
       "https://github.com/coredns/coredns/releases/download/v${COREDNS_VERSION}/coredns_${COREDNS_VERSION}_linux_${GOARCH}.tgz" \
    && echo "${COREDNS_SHA256}  /tmp/coredns.tgz" | sha256sum --check --strict - \
    && tar -xzf /tmp/coredns.tgz -C /usr/local/bin coredns \
    && rm /tmp/coredns.tgz \
    && chmod +x /usr/local/bin/coredns \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && command -v dig >/dev/null

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
