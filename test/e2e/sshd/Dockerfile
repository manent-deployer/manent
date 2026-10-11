# Server SSH per i test end-to-end del plugin remote-copy (scripts/e2e-ssh.sh).
# Debian e non Alpine: il plugin usa GNU coreutils (sha256sum, mv -T, readlink).
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server coreutils tar gzip \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash deploy \
 && mkdir -p /run/sshd /home/deploy/.ssh \
 && chmod 700 /home/deploy/.ssh \
 && chown deploy:deploy /home/deploy/.ssh \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/manent.conf
COPY entrypoint.sh /entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
