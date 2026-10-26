#!/bin/sh
# Builds the demo artifacts in $1: one tar.gz per application and version, with a page showing
# the version and /healthz. shop 1.4.0 is broken on purpose: it has no /healthz.
set -eu
out=$1
mkdir -p "$out"
build() { # build <app> <version> [broken]
  d=$(mktemp -d)
  cat >"$d/index.html" <<HTML
<!doctype html><title>$1 $2</title>
<body style="font:24px system-ui;display:grid;place-items:center;height:90vh"><p><b>$1</b> $2</p>
HTML
  [ "${3:-}" = broken ] || echo ok >"$d/healthz"
  tar -czf "$out/$1-$2.tar.gz" -C "$d" .
  rm -rf "$d"
}
for v in 1.0.0 1.1.0 1.2.0 1.3.0; do build shop $v; done
build shop 1.4.0 broken
for v in 2.2.0 2.3.0 2.4.0; do build api $v; done
for v in 0.9.0 1.0.0; do build billing $v; done
