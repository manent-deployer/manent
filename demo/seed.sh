#!/bin/sh
# Fills the demo with some history, like a few weeks of releases would: every application in
# staging and prod, promotions from staging and a failed deploy. Every deploy has an id, so
# running it again (a restarted container) does not repeat anything.
set -eu
until manent whoami >/dev/null 2>&1; do sleep 1; done

d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
deploy() { # deploy <app> <env> <version>
  cat >"$d/deploy.yml" <<YAML
apiVersion: manent/v1
id: seed-$1-$2-$(echo "$3" | tr . -)
application: $1
environment: $2
artifact:
  type: application
  name: $1
  version: "$3"
  source: file:///demo/artifacts/$1-$3.tar.gz
YAML
  manent deploy --json -f "$d/deploy.yml" >/dev/null 2>&1 || true
}
promote() { # promote <app> <version>
  manent promote --json --id "seed-$1-promote-$(echo "$2" | tr . -)" -a "$1" --from staging --to prod >/dev/null 2>&1 || true
}

deploy shop staging 1.0.0; promote shop 1.0.0
deploy api staging 2.2.0; promote api 2.2.0
deploy billing staging 0.9.0; promote billing 0.9.0
deploy shop staging 1.1.0; promote shop 1.1.0
deploy api staging 2.3.0; promote api 2.3.0
deploy billing staging 1.0.0
deploy shop staging 1.2.0; promote shop 1.2.0
deploy shop staging 1.3.0
deploy api staging 2.4.0
echo "demo history ready"
