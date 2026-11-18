ARG DIND_IMAGE
FROM ${DIND_IMAGE}
USER root
ARG UID=1000
ARG GID=1000
ARG SUBID_START=100000
LABEL com.hole.image=rootless
# Mirror the agent identity; never chown a host bind mount. The base image ships
# rootlesskit, slirp4netns and setuid newuidmap/newgidmap with subordinate ranges.
RUN test "$UID" -gt 0 && test "$GID" -gt 0 \
    && sed -i "s/^rootless:x:1000:1000:/rootless:x:${UID}:${GID}:/" /etc/passwd \
    && sed -i "s/^rootless:x:1000:/rootless:x:${GID}:/" /etc/group \
    && printf 'rootless:%s:65536\n' "$SUBID_START" > /etc/subuid \
    && printf 'rootless:%s:65536\n' "$SUBID_START" > /etc/subgid \
    && mkdir -p /home/rootless/.local/share/docker /run/user/${UID} \
    && chown -R ${UID}:${GID} /home/rootless /run/user/${UID}
COPY launch.sh daemon.sh health.sh hole-dockerd-netns /usr/local/bin/
RUN chmod 755 /usr/local/bin/launch.sh /usr/local/bin/daemon.sh /usr/local/bin/health.sh /usr/local/bin/hole-dockerd-netns
USER rootless
ENV HOME=/home/rootless
ENTRYPOINT ["/usr/local/bin/launch.sh"]
