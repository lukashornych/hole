FROM ubuntu:24.04

ARG UID=1000
ARG GID=1000
ARG AGENT_USERNAME=agent
ARG AGENT_HOME=/home/agent

# The rest of the steps CANNOT be cached, we need to rerun the setup script everytime (there may be changes), and download
# the latest package versions everytime.
ARG CACHEBUST=1

# removes default ubuntu user whose id we need for the agent user
RUN touch /var/mail/ubuntu && chown ubuntu /var/mail/ubuntu && userdel -r ubuntu

# Dependencies installaction

## Base dependencies
RUN apt-get update && apt-get install -y \
    curl \
    git \
    jq \
    ripgrep \
    ca-certificates \
    gnupg \
    sudo \
    && rm -rf /var/lib/apt/lists/*

## User-defined dependencies
ARG EXTRA_PACKAGES=""
RUN if [ -n "$EXTRA_PACKAGES" ]; then \
      apt-get update && apt-get install -y --no-install-recommends $EXTRA_PACKAGES \
      && rm -rf /var/lib/apt/lists/*; \
    fi

## Dependencies to access Docker-in-Docker
ARG DOCKER_ENABLED=""
RUN if [ -n "$DOCKER_ENABLED" ]; then \
      curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg \
      && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list \
      && apt-get update \
      && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
      && rm -rf /var/lib/apt/lists/*; \
    fi

## User setup hook script
COPY setup-scripts/ /tmp/setup-scripts/
RUN if [ -f /tmp/setup-scripts/setup.sh ]; then \
      chmod +x /tmp/setup-scripts/setup.sh \
      && /tmp/setup-scripts/setup.sh; \
    fi \
    && rm -rf /tmp/setup-scripts

# Install Gemini CLI globally (in /usr/local/bin, not agent home which is a persistent volume)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @google/gemini-cli

# Create agent user
RUN groupadd -g "${GID}" "${AGENT_USERNAME}" && \
    useradd -u "${UID}" -g "${GID}" -m -d "${AGENT_HOME}" -s /bin/bash "${AGENT_USERNAME}"
RUN echo "${AGENT_USERNAME} ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/${AGENT_USERNAME}
USER ${AGENT_USERNAME}

CMD ["gemini", "--yolo"]
