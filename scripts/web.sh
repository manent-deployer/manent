#!/bin/sh
# Esegue npm nella cartella web/: con Node >= 20 installato usa quello locale, altrimenti
# un container node:22 (serve solo Docker). Esempi: scripts/web.sh ci; scripts/web.sh run build.
# WEB_PORT=5173 pubblica la porta del server di sviluppo quando si usa il container.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
image=node:22.11.0-bookworm-slim
major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)
if [ "$major" -ge 20 ] && [ -z "${WEB_DOCKER:-}" ]; then
  cd "$root/web" && exec npm "$@"
fi
tty=""; [ -t 0 ] && tty="-it"
ports=""; [ -n "${WEB_PORT:-}" ] && ports="-p 127.0.0.1:${WEB_PORT}:${WEB_PORT}"
exec docker run --rm $tty $ports -e MANENT_DEV_API="${MANENT_DEV_API:-http://host.docker.internal:8080}" \
  -v "$root":/repo -w /repo/web "$image" npm "$@"
