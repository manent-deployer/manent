#!/bin/sh
# Runs npm in the web/ folder: with Node >= 20 installed it uses the local one, otherwise
# a node:22 container (only Docker is needed). Examples: scripts/web.sh ci; scripts/web.sh run build.
# WEB_PORT=5173 publishes the development server port when using the container.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
image=node:22.23.4-bookworm-slim@sha256:4b150b48b4116e2a22f2c39cc27025fbf4f153e2078fd3b14d159fdb5c7279da
major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)
if [ "$major" -ge 20 ] && [ -z "${WEB_DOCKER:-}" ]; then
  cd "$root/web" && exec npm "$@"
fi
tty=""; [ -t 0 ] && tty="-it"
ports=""; [ -n "${WEB_PORT:-}" ] && ports="-p 127.0.0.1:${WEB_PORT}:${WEB_PORT}"
exec docker run --rm $tty $ports -e MANENT_DEV_API="${MANENT_DEV_API:-http://host.docker.internal:8080}" \
  -v "$root":/repo -w /repo/web "$image" npm "$@"
