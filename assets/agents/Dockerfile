ARG BASE_IMAGE=ubuntu:24.04

FROM ${BASE_IMAGE}

LABEL com.hole.image=agent

# Use bash with pipefail so any failed command in a pipe aborts the build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG UID=1000
ARG GID=1000
ARG AGENT_USERNAME=agent
ARG AGENT_HOME=/home/agent

# removes default ubuntu user whose id we need for the agent user
RUN touch /var/mail/ubuntu && chown ubuntu /var/mail/ubuntu && userdel -r ubuntu

# The rest of the steps CANNOT be cached, we need to rerun the setup script everytime (there may be changes), and download
# the latest package versions everytime.
ARG CACHEBUST=1

# Dependencies installaction

## Base dependencies
## iproute2 is required by the entrypoint to point the default route at the gateway
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    git \
    iproute2 \
    jq \
    nano \
    vim \
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
# buildx is not optional in practice: a modern docker CLI routes `docker build` through BuildKit
# and without the plugin falls back to the deprecated legacy builder, while `docker buildx` fails
# outright. The build context is streamed by the client, so builds work even though the daemon
# lives in the sidecar and cannot see the agent's filesystem.
RUN curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg \
      && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list \
      && apt-get update \
      && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin docker-buildx-plugin \
      && rm -rf /var/lib/apt/lists/*;

# Agent CLI installations (root phase — global packages)
COPY agent-installs/ /tmp/agent-installs/
RUN for agent_dir in /tmp/agent-installs/*/; do \
      agent=$(basename "${agent_dir}"); \
      if [ -f "/tmp/agent-installs/${agent}/install-root.sh" ]; then \
        chmod +x "/tmp/agent-installs/${agent}/install-root.sh" \
        && "/tmp/agent-installs/${agent}/install-root.sh"; \
      fi; \
    done

# Create agent user
# -l on useradd avoids a huge sparse /var/log/lastlog entry when host UID is very high
RUN groupadd -g "${GID}" "${AGENT_USERNAME}" \
    && useradd -l -u "${UID}" -g "${GID}" -m -d "${AGENT_HOME}" -s /bin/bash "${AGENT_USERNAME}" \
    && echo "${AGENT_USERNAME} ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/${AGENT_USERNAME}

# Prepare Agent CLI installations (user phase — user-space tools)
RUN for agent_dir in /tmp/agent-installs/*/; do \
      agent=$(basename "${agent_dir}"); \
      if [ -f "/tmp/agent-installs/${agent}/install-user.sh" ]; then \
        chown "${AGENT_USERNAME}" "/tmp/agent-installs/${agent}/install-user.sh" && \
        chmod +x "/tmp/agent-installs/${agent}/install-user.sh"; \
      fi; \
    done

## Prepare user setup hook scripts
COPY setup-scripts/ /tmp/setup-scripts/
RUN chown -R "${AGENT_USERNAME}" /tmp/setup-scripts && chmod -R +x /tmp/setup-scripts

USER ${AGENT_USERNAME}

# Execute Agent CLI installations (user phase — user-space tools)
ENV HOME=${AGENT_HOME}

# support for non-interactive shells (agents run non-interactively), e.g. using NVM in non-interactive shells
ENV BASH_ENV="${AGENT_HOME}/.bash_env"
RUN touch "${AGENT_HOME}/.bash_env"
RUN echo '[ -s "${BASH_ENV}" ] && . "${BASH_ENV}"' >> "${AGENT_HOME}/.bashrc"

RUN for agent_dir in /tmp/agent-installs/*/; do \
      agent=$(basename "${agent_dir}"); \
      if [ -f "/tmp/agent-installs/${agent}/install-user.sh" ]; then \
        "/tmp/agent-installs/${agent}/install-user.sh"; \
      fi; \
    done

## Execute user setup hook scripts in lexicographic order
RUN for script in /tmp/setup-scripts/*.sh; do \
      [ -f "${script}" ] || continue; \
      echo "Running setup hook: $(basename "${script}")..."; \
      "${script}"; \
    done

USER root

# Cleanup install scripts
RUN rm -rf /tmp/agent-installs
RUN rm -rf /tmp/setup-scripts

# Prepare entrypoint script
COPY entrypoint.sh /tmp/entrypoint.sh
RUN chmod +x /tmp/entrypoint.sh

USER ${AGENT_USERNAME}

ENV PATH="${AGENT_HOME}/.local/bin:$PATH"

ENTRYPOINT ["/tmp/entrypoint.sh"]
