#!/bin/sh
# Installa la chiave pubblica passata in AUTHORIZED_KEY e avvia sshd in primo piano.
set -eu
printf '%s\n' "$AUTHORIZED_KEY" > /home/deploy/.ssh/authorized_keys
chown deploy:deploy /home/deploy/.ssh/authorized_keys
chmod 600 /home/deploy/.ssh/authorized_keys
ssh-keygen -A >/dev/null
exec /usr/sbin/sshd -D -e
