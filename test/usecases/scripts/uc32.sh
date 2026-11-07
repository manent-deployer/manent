#!/bin/sh
# UC32: deploy script of the lab catalog (plugins.script), run by the script plugin on
# worker-ansible (the distroless workers have no shell). It releases the bundle, a tar.gz, on
# the targets over SSH in <dest>/<version>, with a "current" file holding the version in
# service, and branches on MANENT_ACTION. Faults are files on the target, made by the case:
#   $HOME/fault/uc32.fail   permanent failure (exit 3)
#   $HOME/fault/uc32.flaky  temporary failure, once (exit 75; the file is removed)
set -eu
dest=${MANENT_PARAM_DEST:-}
key=$MANENT_SECRETS_DIR/deploy-key

# hosts: the targets of the run, one per line.
hosts() {
  if [ -n "${MANENT_TARGET:-}" ]; then echo "$MANENT_TARGET"; return; fi
  python3 -c 'import json, sys; print("\n".join(t["host"] for t in json.load(open(sys.argv[1]))))' "$MANENT_TARGETS"
}
# on <host> <command>: runs the command on the target as deploy.
on() {
  h=$1; shift
  ssh -i "$key" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=10 "deploy@$h" "$@"
}

if [ "$MANENT_SCRIPT" = uc32-slow ]; then
  # Never ends by itself: the timeout must stop it and its child.
  echo "slow script started"
  sleep 300 &
  wait
fi

case "$MANENT_ACTION" in
deploy)
  echo "token in use: $(cat "$MANENT_SECRETS_DIR/api-token")"
  for h in $(hosts); do
    # One run per target: long enough for the case to see two runs overlap.
    [ "$MANENT_SCRIPT" != uc32-each ] || sleep 3
    if on "$h" 'test -e "$HOME/fault/uc32.fail"'; then
      echo "simulated failure on $h" >&2
      exit 3
    fi
    if on "$h" 'test -e "$HOME/fault/uc32.flaky" && rm -f "$HOME/fault/uc32.flaky"'; then
      echo "temporary failure on $h: asking for a retry" >&2
      exit 75
    fi
    on "$h" "mkdir -p '$dest/$MANENT_VERSION' && tar -xzf - -C '$dest/$MANENT_VERSION' &&
      echo '$MANENT_VERSION' > '$dest/current.tmp' && mv '$dest/current.tmp' '$dest/current'" <"$MANENT_ARTIFACT_PATH"
    # What the script received: the case checks the clean environment and the secret file.
    env | cut -d= -f1 | sort | tr '\n' ' ' | on "$h" "cat > '$dest/env'"
    on "$h" "cat > '$dest/token'" <"$MANENT_SECRETS_DIR/api-token"
    echo "$MANENT_DEPLOYMENT_ID $(date +%s)" | on "$h" "cat >> '$dest/runs'"
    echo "released $MANENT_VERSION on $h"
  done
  {
    echo "endpoint=http://$(hosts | head -1):8080/uc32"
    echo "parallelism=$MANENT_PARALLELISM"
  } >>"$MANENT_OUTPUT"
  ;;
dry-run)
  for h in $(hosts); do
    on "$h" "test -w /home/deploy"
    echo "dry-run: $h reachable, $MANENT_VERSION would go to $dest"
  done
  ;;
current)
  v=$(on "$(hosts | head -1)" "cat '$dest/current' 2>/dev/null || true")
  [ -z "$v" ] || echo "version=$v" >>"$MANENT_OUTPUT"
  ;;
rollback)
  for h in $(hosts); do
    on "$h" "test -d '$dest/$MANENT_PREVIOUS_VERSION' && echo '$MANENT_PREVIOUS_VERSION' > '$dest/current'"
    echo "rolled back $h to $MANENT_PREVIOUS_VERSION"
  done
  ;;
esac
