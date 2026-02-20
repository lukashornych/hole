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

RUN useradd -m -s /bin/bash claude
RUN echo "claude ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/claude

WORKDIR /workspace

# Staging dir for .claude.json (mounted read-only, copied to home by entrypoint)
RUN mkdir -p /home/claude/.host-config \
    && chown -R claude:claude /home/claude/.host-config

COPY agents/claude/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER claude

RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/home/claude/.local/bin:$PATH"

ENTRYPOINT ["entrypoint.sh"]
