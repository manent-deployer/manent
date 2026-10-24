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
[ -n "$cases" ] || cases=$(cd cases && ls uc*.sh | sort -t c -k2 -n | sed 's/\.sh$//' | tr '\n' ' ')

cleanup() {
  if [ "$keep" = 1 ]; then
    echo "laboratorio attivo: docker compose -f test/usecases/compose.yml --profile regions --profile containers --profile kubernetes --profile lb --profile jboss --profile metrics --profile terraform --profile windows --profile git down -v"
  else
    dc --profile regions --profile tools --profile containers --profile kubernetes --profile lb --profile jboss --profile metrics --profile terraform --profile windows --profile git down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log "preparazione"
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/keys" "$WORK/artifacts" "$WORK/uc" "$WORK/config" "$WORK/k3s" "$WORK/ci" "$WORK/tf"
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in x86_64|amd64) goarch=amd64 ;; aarch64|arm64) goarch=arm64 ;; *) goarch=$arch ;; esac
# Web interface embedded in manent-server (UC8): local Node >= 20 or a container.
(cd ../.. && { [ -f web/node_modules/.package-lock.json ] || scripts/web.sh ci --no-audit --no-fund >/dev/null; } && scripts/web.sh run build >/dev/null 2>&1) ||
  { echo "build dell'interfaccia web fallita (make web)" >&2; exit 1; }
info "interfaccia web compilata"
(cd ../.. && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/" ./cmd/manent ./cmd/manent-server &&
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-uc-oidc" ./test/usecases/oidc &&
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-uc-fakes" ./test/usecases/fakes &&
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-uc-git" ./test/usecases/gitserver &&
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-uc-windows" ./test/usecases/windows/fake &&
  cd integrations/terraform && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/terraform-provider-manent" .)
# Terraform plugin (UC28): from the checkout next to manent if present, else the released module.
tfplugin=${MANENT_PLUGIN_TERRAFORM:-../../../manent-plugin-terraform}
if [ -f "$tfplugin/go.mod" ]; then
  (cd "$tfplugin" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/manent-plugin-terraform" .)
else
  (cd "$WORK" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOBIN= GOPATH="$WORK/gopath" \
    go install -trimpath "github.com/manent-deployer/manent-plugin-terraform@${TERRAFORM_PLUGIN_VERSION:-v0.1.0}" &&
    cp "$(find "$WORK/gopath/bin" -name manent-plugin-terraform -type f | head -1)" "$WORK/bin/")
fi || { echo "build del plugin terraform fallita" >&2; exit 1; }
info "binari linux/$goarch compilati"

# Lab SSH key: readable by the nonroot user of the manent containers.
ssh-keygen -q -t ed25519 -N '' -C manent-uc -f "$WORK/keys/id"
chmod 644 "$WORK/keys/id"
# Test secrets read through file: references (UC10, UC12, UC13), before API and workers start.
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$WORK/keys/spec-key"
printf 'admin:uc-dataplane' >"$WORK/keys/dataplane"
printf 'uc-W1ndows!' >"$WORK/keys/win-pass"
printf 'uc21-job-pass' >"$WORK/keys/job-pass"
printf 'uc-azure-secret' >"$WORK/keys/azure-secret"
printf 's3cret-uc12-db' >"$WORK/keys/db-pass"
printf 'tok-uc12-hook' >"$WORK/keys/hook-token"
printf 'xoxb-uc-test' >"$WORK/keys/slack-token"
printf 'postgres://manent:manent@postgres:5432/manent?sslmode=disable' >"$WORK/keys/pg-conn"
printf 'uc-slack-signing' >"$WORK/keys/slack-signing"
printf 'http://fakes:9100/teams/cab' >"$WORK/keys/teams-webhook"
printf 'uc-snow' >"$WORK/keys/snow-password"
printf 'uc-jboss-1' >"$WORK/keys/jboss-pass"
printf 'uc-dd-api' >"$WORK/keys/dd-api"
printf 'uc-dd-app' >"$WORK/keys/dd-app"
printf 'uc-nr-key' >"$WORK/keys/nr-key"
printf 'uc-es-key' >"$WORK/keys/es-key"
printf 'AKIDUCTEST' >"$WORK/keys/aws-id"
printf 'uc-aws-secret' >"$WORK/keys/aws-secret"
# UC27: token of the lab Git server and SSH key that signs the configuration commits.
printf 'uc-git-token' >"$WORK/keys/git-token"
ssh-keygen -q -t ed25519 -N '' -C uc27-signer -f "$WORK/keys/git-sign"
printf 'uc27@example.com namespaces="git" %s\n' "$(cat "$WORK/keys/git-sign.pub")" >"$WORK/keys/git-allowed-signers"
chmod 644 "$WORK"/keys/*

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

dc --profile regions --profile tools --profile containers --profile kubernetes --profile lb --profile jboss --profile metrics --profile terraform --profile windows --profile git down -v -t 5 --remove-orphans >/dev/null 2>&1 || true
dc --profile regions --profile containers --profile lb --profile jboss --profile windows --profile git build -q
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
