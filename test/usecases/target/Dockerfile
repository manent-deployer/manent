# Use case target: SSH server for the remote-copy plugin plus an HTTP server exposing the
# released applications (http://<host>:8080/<app>/current/healthz), used by post-test and
# health gate. Debian and not Alpine: the plugin uses GNU coreutils (sha256sum, mv -T, readlink).
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server coreutils tar gzip busybox \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash deploy \
 && mkdir -p /run/sshd /home/deploy/.ssh /home/deploy/apps /home/deploy/fault \
 && chmod 700 /home/deploy/.ssh \
 && chown -R deploy:deploy /home/deploy \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/manent.conf
COPY entrypoint.sh /entrypoint.sh
EXPOSE 22 8080
ENTRYPOINT ["/entrypoint.sh"]
