#!/bin/sh
# Test end-to-end del plugin remote-copy contro un server SSH reale in Docker.
# Uso: scripts/e2e-ssh.sh  (richiede docker e ssh-keygen)
set -eu
port=${MANENT_E2E_SSH_PORT:-2222}
name=manent-e2e-sshd
dir=$(mktemp -d)
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT

ssh-keygen -q -t ed25519 -N '' -f "$dir/id"
docker build -q -t manent-e2e-sshd test/e2e/sshd >/dev/null
docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" -p "127.0.0.1:$port:22" -e AUTHORIZED_KEY="$(cat "$dir/id.pub")" manent-e2e-sshd >/dev/null

# Attende che sshd accetti connessioni.
i=0
until docker exec "$name" sh -c 'test -s /etc/ssh/ssh_host_ed25519_key.pub' 2>/dev/null && nc -z 127.0.0.1 "$port" 2>/dev/null; do
  i=$((i + 1)); [ "$i" -gt 30 ] && { docker logs "$name"; exit 1; }
  sleep 1
done

MANENT_TEST_SSH_ADDR=127.0.0.1:$port MANENT_TEST_SSH_USER=deploy MANENT_TEST_SSH_KEY=$dir/id \
  go test -count=1 -run TestE2E -v ./plugins/remotecopy/
