FROM ubuntu:24.04

ARG UID=1000
ARG GID=1000

RUN apt-get update && apt-get install -y \
    curl \
    git \
    jq \
    ripgrep \
    ca-certificates \
    gnupg \
    sudo \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -o -g "${GID}" agent && \
    useradd -o -u "${UID}" -g "${GID}" -m -s /bin/bash agent
RUN echo "agent ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/agent

WORKDIR /workspace

USER agent

RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/home/agent/.local/bin:$PATH"

USER root
ARG EXTRA_PACKAGES=""
RUN if [ -n "$EXTRA_PACKAGES" ]; then \
      apt-get update && apt-get install -y --no-install-recommends $EXTRA_PACKAGES \
      && rm -rf /var/lib/apt/lists/*; \
    fi

COPY --from=setup-context . /tmp/setup-scripts/
RUN if [ -f /tmp/setup-scripts/setup.sh ]; then \
      chmod +x /tmp/setup-scripts/setup.sh \
      && /tmp/setup-scripts/setup.sh; \
    fi \
    && rm -rf /tmp/setup-scripts
USER agent

CMD ["claude", "--dangerously-skip-permissions"]
