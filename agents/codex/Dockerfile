FROM ubuntu:24.04

# removes default ubuntu user whose id we need for the agent user
RUN touch /var/mail/ubuntu && chown ubuntu /var/mail/ubuntu && userdel -r ubuntu

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

# Install Codex CLI globally (in /usr/local/bin, not /home/agent which is a persistent volume)
RUN npm install -g @openai/codex

RUN groupadd -g "${GID}" agent && \
    useradd -u "${UID}" -g "${GID}" -m -s /bin/bash agent
RUN echo "agent ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/agent

USER root
ARG EXTRA_PACKAGES=""
RUN if [ -n "$EXTRA_PACKAGES" ]; then \
      apt-get update && apt-get install -y --no-install-recommends $EXTRA_PACKAGES \
      && rm -rf /var/lib/apt/lists/*; \
    fi

ARG DOCKER_ENABLED=""
RUN if [ -n "$DOCKER_ENABLED" ]; then \
      curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg \
      && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list \
      && apt-get update \
      && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
      && rm -rf /var/lib/apt/lists/*; \
    fi

COPY setup-scripts/ /tmp/setup-scripts/
RUN if [ -f /tmp/setup-scripts/setup.sh ]; then \
      chmod +x /tmp/setup-scripts/setup.sh \
      && /tmp/setup-scripts/setup.sh; \
    fi \
    && rm -rf /tmp/setup-scripts
USER agent

CMD ["codex", "--dangerously-bypass-approvals-and-sandbox"]
