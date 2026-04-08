FROM alpine:3.19

ARG COREDNS_VERSION=1.12.1

RUN apk add --no-cache netcat-openbsd curl \
    && ARCH=$(uname -m) \
    && case "${ARCH}" in \
         x86_64)  GOARCH=amd64 ;; \
         aarch64) GOARCH=arm64 ;; \
         *)       echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/coredns/coredns/releases/download/v${COREDNS_VERSION}/coredns_${COREDNS_VERSION}_linux_${GOARCH}.tgz" \
       | tar -xz -C /usr/local/bin \
    && chmod +x /usr/local/bin/coredns \
    && apk del curl

COPY dns-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
