FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    curl \
    git \
    jq \
    ripgrep \
    ca-certificates \
    gnupg \
    sudo \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash agent
RUN echo "agent ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/agent

WORKDIR /workspace

COPY agents/claude/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER agent

RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/home/agent/.local/bin:$PATH"

ENTRYPOINT ["entrypoint.sh"]
