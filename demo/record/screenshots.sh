#!/bin/sh
# Regenerates the screenshots of the web interface (docs/content/assets/screenshots, English and
# Italian, used by the README and the documentation). It starts a fresh demo container with the
# layer demo/record/shots/manent.yml and runs the deploys of the GIF (shop 1.3.0 in waves,
# shop 1.4.0 rolled back by its health gate), one deploy waiting for an approval and one
# scheduled, then takes the screenshots with demo/record/screenshots.cjs.
#
#   docker build -f demo/Dockerfile -t manent-demo .
#   demo/record/screenshots.sh
#
# Requires Docker, Node with web/node_modules installed and Google Chrome (used by Playwright,
# no browser download).
set -eu

cd "$(dirname "$0")/../.."
IMAGE=${IMAGE:-manent-demo}
PORT=${PORT:-18080}
NAME=manent-demo-shots
OUT=${OUT:-docs/content/assets/screenshots}
# KEEP=1 leaves the container running (http://localhost:$PORT) to look at the pages.
trap '[ -n "${KEEP:-}" ] || docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

echo "starting the demo container and waiting for its history..."
shots=$(pwd)/demo/record/shots
docker rm -f "$NAME" >/dev/null 2>&1 || true
# The identity provider keeps port 9000: it is the issuer the server and the browser both use.
docker run -d --rm -p "$PORT:8080" -p 9000:9000 --name "$NAME" \
  -e MANENT_CONFIG=/etc/manent/manent.yml,/demo/shots/manent.yml \
  -v "$shots/manent.yml:/demo/shots/manent.yml:ro" \
  -v "$shots/billing-reports-prod.yml:/demo/envconfig/untenanted/billing/billing-reports/prod.yml:ro" \
  "$IMAGE" >/dev/null
until docker logs "$NAME" 2>&1 | grep -q "demo history ready"; do sleep 2; done

run() { docker exec -w /demo/deploys "$NAME" "$@"; }
echo "deploying shop 1.3.0 in waves and shop 1.4.0 (rolled back)..."
run manent deploy -f shop-1.3.0.yml --id shop-1.3.0 >/dev/null
run manent deploy -f shop-1.4.0.yml --id shop-1.4.0 >/dev/null || true

echo "queueing a deploy waiting for an approval and a scheduled one..."
# GNU date in Linux, BSD date in macOS.
at=$(date -u -d '+2 days' +%Y-%m-%dT10:00:00Z 2>/dev/null || date -u -v+2d +%Y-%m-%dT10:00:00Z)
docker exec -i "$NAME" sh -c 'cat >/tmp/billing-reports.yml' <<YAML
apiVersion: manent/v1
id: billing-reports-1-0-0-prod
application: billing
environment: prod
artifact:
  type: application
  name: billing-reports
  version: "1.0.0"
  source: file:///demo/artifacts/billing-1.0.0.tar.gz
YAML
docker exec -i "$NAME" sh -c 'cat >/tmp/api-scheduled.yml' <<YAML
apiVersion: manent/v1
id: api-2-4-0-prod
application: api
environment: prod
artifact:
  type: application
  name: api
  version: "2.4.0"
  source: file:///demo/artifacts/api-2.4.0.tar.gz
schedule:
  at: "$at"
YAML
run manent deploy --no-wait -f /tmp/billing-reports.yml >/dev/null
run manent deploy --no-wait -f /tmp/api-scheduled.yml >/dev/null

echo "taking the screenshots..."
node demo/record/screenshots.cjs "http://localhost:$PORT" "$OUT"
ls -l "$OUT"
