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

# Install Node.js 22 LTS via NodeSource
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install Gemini CLI globally (in /usr/local/bin, not /home/agent which is a persistent volume)
RUN npm install -g @google/gemini-cli

RUN groupadd -o -g "${GID}" agent && \
    useradd -o -u "${UID}" -g "${GID}" -m -s /bin/bash agent
RUN echo "agent ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/agent

WORKDIR /workspace

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

CMD ["gemini", "--yolo"]
