FROM ubuntu:22.04

RUN apt-get update && apt-get install -y \
    curl \
    git \
    ripgrep \
    ca-certificates \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash claude

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
