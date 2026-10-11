#!/bin/sh
# Installa la chiave pubblica condivisa, avvia sshd e il demone Docker (in primo piano).
set -eu
until [ -s /keys/id.pub ]; do sleep 1; done
cp /keys/id.pub /home/deploy/.ssh/authorized_keys
chown deploy:deploy /home/deploy/.ssh/authorized_keys
chmod 600 /home/deploy/.ssh/authorized_keys
ssh-keygen -A >/dev/null
/usr/sbin/sshd -e
exec dind dockerd --host=unix:///var/run/docker.sock --group=docker --insecure-registry=registry:5000
