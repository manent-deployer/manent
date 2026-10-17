#!/bin/sh
# Runs manent for the GitHub Action (action.yml): the inputs arrive as INPUT_* variables, server
# and token as MANENT_SERVER and MANENT_TOKEN. The use cases (UC18) run it with the same
# variables, as a runner would.
set -eu

case "${INPUT_COMMAND:-deploy}" in
deploy)
  [ -n "${INPUT_DESCRIPTOR:-}" ] || { echo "::error::manca l'input descriptor"; exit 2; }
  set -- deploy --ci -f "$INPUT_DESCRIPTOR"
  if [ -n "${INPUT_VERSION:-}" ]; then set -- "$@" --version "$INPUT_VERSION"; fi
  if [ -n "${INPUT_SOURCE:-}" ]; then set -- "$@" --source "$INPUT_SOURCE"; fi
  if [ -n "${INPUT_SHA256:-}" ]; then set -- "$@" --sha256 "$INPUT_SHA256"; fi
  if [ -n "${INPUT_ID:-}" ]; then set -- "$@" --id "$INPUT_ID"; fi
  if [ "${INPUT_DRY_RUN:-false}" = true ]; then set -- "$@" --dry-run; fi
  if [ "${INPUT_WAIT:-true}" = false ]; then set -- "$@" --no-wait; fi
  if [ -n "${INPUT_TIMEOUT:-}" ]; then set -- "$@" --timeout "$INPUT_TIMEOUT"; fi
  for a in ${INPUT_AFTER:-}; do set -- "$@" --after "$a"; done
  ;;
validate)
  [ -n "${INPUT_DESCRIPTOR:-}" ] || { echo "::error::manca l'input descriptor"; exit 2; }
  set -- validate -f "$INPUT_DESCRIPTOR"
  ;;
env-validate)
  if [ -z "${INPUT_APPLICATION:-}" ] || [ -z "${INPUT_ENVIRONMENT:-}" ] || [ -z "${INPUT_ENV_CONFIG:-}" ]; then
    echo "::error::env-validate richiede application, environment ed env-config"; exit 2
  fi
  set -- env validate -a "$INPUT_APPLICATION" -e "$INPUT_ENVIRONMENT" -f "$INPUT_ENV_CONFIG"
  if [ -n "${INPUT_DESCRIPTOR:-}" ]; then set -- "$@" -d "$INPUT_DESCRIPTOR"; fi
  ;;
*)
  echo "::error::command ${INPUT_COMMAND} non valido (deploy, validate, env-validate)"; exit 2
  ;;
esac
if [ -n "${INPUT_CA_FILE:-}" ]; then set -- --ca-file "$INPUT_CA_FILE" "$@"; fi
exec "${MANENT_BIN:-manent}" "$@"
