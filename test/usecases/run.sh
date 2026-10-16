#!/bin/sh
# manent end-to-end use cases on Docker Compose: API server, workers, client and SSH targets.
#
# Usage: test/usecases/run.sh [--keep] [uc1 uc2 ...]   (default: all cases, in order)
#   --keep   leaves the lab running at the end (docker compose -f test/usecases/compose.yml down -v)
#
# Requires docker (compose v2), go, jq and ssh-keygen.
set -eu
cd "$(dirname "$0")"
. ./lib.sh

keep=0
cases=""
for a in "$@"; do
  case "$a" in
    --keep) keep=1 ;;
    uc[0-9]*) cases="$cases $a" ;;
    *) echo "argomento sconosciuto: $a" >&2; exit 2 ;;
  esac
done
[ -n "$cases" ] || cases=$(cd cases && ls uc*.sh | sed 's/\.sh$//' | tr '\n' ' ')

cleanup() {
  if [ "$keep" = 1 ]; then
    echo "laboratorio attivo: docker compose -f test/usecases/compose.yml --profile regions --profile containers --profile kubernetes down -v"
  else
    dc --profile regions --profile tools --profile containers --profile kubernetes down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log "preparazione"
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/keys" "$WORK/artifacts" "$WORK/uc" "$WORK/config" "$WORK/k3s"
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in x86_64|amd64) goarch=amd64 ;; aarch64|arm64) goarch=arm64 ;; *) goarch=$arch ;; esac
# Web interface embedded in manent-server (UC8): local Node >= 20 or a container.
(cd ../.. && { [ -f web/node_modules/.package-lock.json ] || scripts/web.sh ci --no-audit --no-fund >/dev/null; } && scripts/web.sh run build >/dev/null 2>&1) ||
  { echo "build dell'interfaccia web fallita (make web)" >&2; exit 1; }
info "interfaccia web compilata"
(cd ../.. && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/" ./cmd/manent ./cmd/manent-server &&
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-uc-oidc" ./test/usecases/oidc)
info "binari linux/$goarch compilati"

# Lab SSH key: readable by the nonroot user of the manent containers.
ssh-keygen -q -t ed25519 -N '' -C manent-uc -f "$WORK/keys/id"
chmod 644 "$WORK/keys/id"

# Configuration variants for the region workers (UC5).
region_config() {
  awk -v region="$2" '{ print } /^dbos:/ { if (region != "") print "  region: " region; print "  region_routing: true" }' \
    config/manent.yml >"$WORK/config/$1.yml"
}
# k3s cluster credentials (UC7): static token with cluster administrator permissions.
printf 'uc-k8s-token\n' >"$WORK/k3s/token"
printf 'uc-k8s-token,manent,manent,system:masters\n' >"$WORK/k3s/tokens.csv"

region_config orchestrator ""
region_config worker-eu1 eu-1
region_config worker-us1 us-1

dc --profile regions --profile tools --profile containers --profile kubernetes down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
dc --profile regions --profile containers build -q
dc up -d --quiet-pull >/dev/null 2>&1 || dc up -d
t=0
until curl -fs http://127.0.0.1:18080/readyz >/dev/null 2>&1; do
  t=$((t + 1)); [ "$t" -gt 60 ] && { dc logs api | tail -30; exit 1; }
  sleep 1
done
info "laboratorio avviato: $(dc ps --format '{{.Service}}' | sort | tr '\n' ' ')"

# In the cases a failed command does not stop the suite: what counts are the checks (pass/fail).
set +e
for c in $cases; do
  CASE=$c
  . "./cases/$c.sh"
done

log "riepilogo"
printf '   superati: %s, falliti: %s\n' "$PASSED" "$FAILED"
if [ "$FAILED" -gt 0 ]; then
  printf "   fallimenti:$FAILURES\n"
  dc logs --no-color >"$WORK/compose.log" 2>&1 || true
  echo "   log dei container: test/usecases/.work/compose.log"
  exit 1
fi
