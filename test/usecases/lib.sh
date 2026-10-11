# Funzioni comuni dei casi d'uso (incluso da run.sh). Le azioni passano dal client manent
# (docker compose run client), le verifiche leggono lo stato dal client e i target con docker exec.

UC_DIR=$(pwd)  # run.sh esegue i casi dalla directory test/usecases
WORK=$UC_DIR/.work
PASSED=0
FAILED=0
FAILURES=""

dc() { docker compose -f "$UC_DIR/compose.yml" "$@"; }

# as <ruolo> <comando...>: esegue il comando con il token di un altro ruolo (cab, admin, viewer).
# Subshell: in sh POSIX "AS=x funzione" lascerebbe AS impostata dopo la chiamata.
as() { (AS=$1; shift; "$@"); }

# manent esegue il client con il token di $AS (ci, cab, admin, viewer; default ci).
manent() { dc run --rm -T -e MANENT_TOKEN="uc-${AS:-ci}-token" client "$@" 2>>"$WORK/client.log"; }

# manent_err esegue il client e restituisce lo stderr (per verificare gli errori attesi).
manent_err() { dc run --rm -T -e MANENT_TOKEN="uc-${AS:-ci}-token" client "$@" 2>&1 >/dev/null | grep -v -e '^ *Container' -e '^time='; }

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }

pass() { PASSED=$((PASSED + 1)); printf '   \033[32m✔\033[0m %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1)); FAILURES="$FAILURES\n - [$CASE] $1"
  printf '   \033[31m✘ %s\033[0m\n' "$1"
  [ -n "${2:-}" ] && printf '     %s\n' "$2"
  return 0
}

# expect_eq <descrizione> <valore> <atteso>
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "atteso '$3', ottenuto '$2'"; fi
}

# expect_match <descrizione> <testo> <regex estesa>
expect_match() {
  if printf '%s' "$2" | grep -Eq -- "$3"; then pass "$1"; else fail "$1" "'$3' non trovato in: $(printf '%s' "$2" | head -c 400)"; fi
}

# at_in <secondi>: orario RFC 3339 (UTC) tra <secondi>.
at_in() { date -u -v+"$1"S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "+$1 seconds" +%Y-%m-%dT%H:%M:%SZ; }

# artifact <app> <versione> [broken]: crea il tar.gz in .work/artifacts e stampa lo sha256.
# Un artefatto "broken" non contiene healthz: post-test e health gate lo vedono rosso.
artifact() {
  d=$(mktemp -d)
  printf '%s\n' "$2" >"$d/version"
  [ "${3:-}" = broken ] || printf 'ok %s\n' "$2" >"$d/healthz"
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/$1-$2.tar.gz" -C "$d" .
  rm -rf "$d"
  sha "$WORK/artifacts/$1-$2.tar.gz"
}
sha() { shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1 || sha256sum "$1" | cut -d' ' -f1; }

# descriptor <file> <id> <app> <env> <versione> [yaml aggiuntivo]: scrive .work/uc/<file>.yml
# e stampa il percorso nel container del client.
descriptor() {
  cat >"$WORK/uc/$1.yml" <<EOF
apiVersion: manent/v1
id: $2
application: $3
environment: $4
artifact:
  type: application
  name: $3
  version: "$5"
  source: file:///srv/artifacts/$3-$5.tar.gz
${6:-}
EOF
  echo "/uc/$1.yml"
}

# submit <descrittore> [opzioni]: accoda senza attendere e stampa l'ID.
submit() { manent deploy --no-wait -f "$@" | jq -r .id; }

state() { manent status "$1" | jq -r .state; }

# wait_state <id> <stato|terminal> [timeout]: attende lo stato e lo stampa.
wait_state() {
  id=$1 want=$2 limit=${3:-120} t=0 s=""
  while [ "$t" -lt "$limit" ]; do
    s=$(state "$id")
    case "$want" in
      terminal) case "$s" in SUCCESS|ERROR|CANCELLED|REJECTED|EXPIRED|MAX_RECOVERY_ATTEMPTS_EXCEEDED) echo "$s"; return 0 ;; esac ;;
      *) [ "$s" = "$want" ] && { echo "$s"; return 0; } ;;
    esac
    sleep 2; t=$((t + 3))
  done
  echo "$s"
}

# wait_rollout <id> <stato del rollout> [timeout]
wait_rollout() {
  id=$1 want=$2 limit=${3:-120} t=0 s=""
  while [ "$t" -lt "$limit" ]; do
    s=$(manent status "$id" | jq -r '.rollout.status // empty')
    [ "$s" = "$want" ] && { echo "$s"; return 0; }
    sleep 2; t=$((t + 3))
  done
  echo "$s"
}

# current <target> <app>: versione puntata dal symlink current ("none" se assente).
current() {
  v=$(dc exec -T "$1" readlink "/home/deploy/apps/$2/current" 2>/dev/null) || { echo none; return; }
  basename "$v"
}

# currents <app> <target>...: versioni dei target separate da spazi.
currents() { app=$1; shift; out=""; for h in "$@"; do out="$out $(current "$h" "$app")"; done; echo "${out# }"; }

# healthy <target> <app>: 0 se /healthz della release attiva risponde 200.
healthy() { dc exec -T "$1" busybox wget -q -O /dev/null "http://127.0.0.1:8080/$2/current/healthz" 2>/dev/null; }

# fault <target> <app> fail|sleep <s>|clear: guasti simulati letti dal post_command.
fault() {
  f=/home/deploy/fault/$2
  case "$3" in
    fail) dc exec -T -u deploy "$1" touch "$f.fail" ;;
    sleep) dc exec -T -u deploy "$1" sh -c "echo $4 > $f.sleep" ;;
    clear) dc exec -T -u deploy "$1" rm -f "$f.fail" "$f.sleep" ;;
  esac
}

# runs <target> <app>: righe "start" del post_command (esecuzioni del deploy sul target).
runs() { dc exec -T "$1" sh -c "cat /home/deploy/fault/$2.log 2>/dev/null | grep -c start || true"; }

# steps <id>: nomi degli step eseguiti, separati da spazi.
step_names() { manent status "$1" | jq -r '[.result.steps[]?.name] | join(" ")'; }
