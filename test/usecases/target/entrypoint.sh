#!/bin/sh
# Installs the shared public key (/keys/id.pub), starts the applications' HTTP server as the
# deploy user and sshd in the foreground.
set -eu
until [ -s /keys/id.pub ]; do sleep 1; done
cp /keys/id.pub /home/deploy/.ssh/authorized_keys
chown deploy:deploy /home/deploy/.ssh/authorized_keys
chmod 600 /home/deploy/.ssh/authorized_keys
ssh-keygen -A >/dev/null
su deploy -s /bin/sh -c 'busybox httpd -p 8080 -h /home/deploy/apps'
exec /usr/sbin/sshd -D -e
