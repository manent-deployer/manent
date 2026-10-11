#!/bin/sh
# Casi d'uso end-to-end di manent su Docker Compose: server API, worker, client e target SSH.
#
# Uso: test/usecases/run.sh [--keep] [uc1 uc2 ...]   (default: tutti i casi, in ordine)
#   --keep   lascia il laboratorio attivo alla fine (docker compose -f test/usecases/compose.yml down -v)
#
# Richiede docker (compose v2), go, jq e ssh-keygen.
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
    echo "laboratorio attivo: docker compose -f test/usecases/compose.yml --profile regions down -v"
  else
    dc --profile regions --profile tools down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log "preparazione"
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/keys" "$WORK/artifacts" "$WORK/uc" "$WORK/config"
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in x86_64|amd64) goarch=amd64 ;; aarch64|arm64) goarch=arm64 ;; *) goarch=$arch ;; esac
(cd ../.. && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/" ./cmd/manent ./cmd/manent-server)
info "binari linux/$goarch compilati"

# Chiave SSH del laboratorio: leggibile dall'utente nonroot dei container di manent.
ssh-keygen -q -t ed25519 -N '' -C manent-uc -f "$WORK/keys/id"
chmod 644 "$WORK/keys/id"

# Varianti della configurazione per i worker di region (UC5).
region_config() {
  awk -v region="$2" '{ print } /^dbos:/ { if (region != "") print "  region: " region; print "  region_routing: true" }' \
    config/manent.yml >"$WORK/config/$1.yml"
}
region_config orchestrator ""
region_config worker-eu1 eu-1
region_config worker-us1 us-1

dc --profile regions --profile tools down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
dc --profile regions build -q
dc up -d --quiet-pull >/dev/null 2>&1 || dc up -d
t=0
until curl -fs http://127.0.0.1:18080/readyz >/dev/null 2>&1; do
  t=$((t + 1)); [ "$t" -gt 60 ] && { dc logs api | tail -30; exit 1; }
  sleep 1
done
info "laboratorio avviato: $(dc ps --format '{{.Service}}' | sort | tr '\n' ' ')"

# Nei casi un comando fallito non interrompe la suite: contano le verifiche (pass/fail).
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
