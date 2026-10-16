# Shared use case functions (sourced by run.sh). Actions go through the manent client
# (docker compose run client), checks read the state from the client and the targets with docker exec.

UC_DIR=$(pwd)  # run.sh runs the cases from the test/usecases directory
WORK=$UC_DIR/.work
PASSED=0
FAILED=0
FAILURES=""

dc() { docker compose -f "$UC_DIR/compose.yml" "$@"; }

# dcp: like dc, with the containers and kubernetes profiles enabled (registry, VMs and k3s of UC6/UC7).
dcp() { dc --profile containers --profile kubernetes "$@"; }

# as <role> <command...>: runs the command with another role's token (cab, admin, viewer).
# Subshell: in POSIX sh "AS=x function" would leave AS set after the call.
as() { (AS=$1; shift; "$@"); }

# manent runs the client with $AS's token (ci, cab, admin, viewer; default ci).
manent() { dc run --rm -T -e MANENT_TOKEN="uc-${AS:-ci}-token" client "$@" 2>>"$WORK/client.log"; }

# manent_err runs the client and returns stderr (to check expected errors).
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

# expect_eq <description> <value> <expected>
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "atteso '$3', ottenuto '$2'"; fi
}

# expect_match <description> <text> <extended regex>
expect_match() {
  if printf '%s' "$2" | grep -Eq -- "$3"; then pass "$1"; else fail "$1" "'$3' non trovato in: $(printf '%s' "$2" | head -c 400)"; fi
}

# at_in <seconds>: RFC 3339 time (UTC) <seconds> from now.
at_in() { date -u -v+"$1"S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "+$1 seconds" +%Y-%m-%dT%H:%M:%SZ; }

# artifact <app> <version> [broken]: creates the tar.gz in .work/artifacts and prints its sha256.
# A "broken" artifact has no healthz: post-test and health gate see it red.
artifact() {
  d=$(mktemp -d)
  printf '%s\n' "$2" >"$d/version"
  [ "${3:-}" = broken ] || printf 'ok %s\n' "$2" >"$d/healthz"
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/$1-$2.tar.gz" -C "$d" .
  rm -rf "$d"
  sha "$WORK/artifacts/$1-$2.tar.gz"
}
sha() { shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1 || sha256sum "$1" | cut -d' ' -f1; }

# descriptor <file> <id> <app> <env> <version> [extra yaml]: writes .work/uc/<file>.yml
# for an application artifact and prints its path in the client container.
descriptor() {
  art_descriptor "$1" "$2" "$3" "$4" "$5" application "file:///srv/artifacts/$3-$5.tar.gz" "${6:-}"
}

# art_descriptor <file> <id> <app> <env> <version> <type> <source> [extra yaml]
art_descriptor() {
  cat >"$WORK/uc/$1.yml" <<EOF
apiVersion: manent/v1
id: $2
application: $3
environment: $4
artifact:
  type: $6
  name: $3
  version: "$5"
  source: $7
${8:-}
EOF
  echo "/uc/$1.yml"
}

# image <name> <tag> [broken] [version]: builds the test application (webapp), pushes it
# to the lab registry as uc/<name>:<tag> and prints the digest (without sha256:).
# A "broken" image has no /healthz: HEALTHCHECK and readinessProbe stay red.
image() {
  healthy=true; [ "${3:-}" = broken ] && healthy=false
  ref=127.0.0.1:15000/uc/$1:$2
  docker build -q --build-arg VERSION="${4:-$2}" --build-arg HEALTHY=$healthy -t "$ref" "$UC_DIR/webapp" >/dev/null &&
    docker push -q "$ref" >/dev/null &&
    docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | grep "^127.0.0.1:15000/uc/$1@" | head -1 | sed 's/.*@sha256://'
}

# wait_registry: waits for the lab registry to answer from the host.
wait_registry() {
  t=0; until curl -fs http://127.0.0.1:15000/v2/ >/dev/null 2>&1; do t=$((t + 1)); [ "$t" -gt 60 ] && return 1; sleep 1; done
}

# submit <descriptor> [options]: enqueues without waiting and prints the ID.
submit() { manent deploy --no-wait -f "$@" | jq -r .id; }

state() { manent status "$1" | jq -r .state; }

# wait_state <id> <state|terminal> [timeout]: waits for the state and prints it.
wait_state() {
  id=$1 want=$2 limit=${3:-120} t=0 s=""
  while [ "$t" -lt "$limit" ]; do
    s=$(state "$id")
    case "$want" in
      terminal) case "$s" in SUCCESS|ERROR|CANCELLED|REJECTED|EXPIRED|DEPENDENCY_FAILED|MAX_RECOVERY_ATTEMPTS_EXCEEDED) echo "$s"; return 0 ;; esac ;;
      *) [ "$s" = "$want" ] && { echo "$s"; return 0; } ;;
    esac
    sleep 2; t=$((t + 3))
  done
  echo "$s"
}

# wait_rollout <id> <rollout state> [timeout]
wait_rollout() {
  id=$1 want=$2 limit=${3:-120} t=0 s=""
  while [ "$t" -lt "$limit" ]; do
    s=$(manent status "$id" | jq -r '.rollout.status // empty')
    [ "$s" = "$want" ] && { echo "$s"; return 0; }
    sleep 2; t=$((t + 3))
  done
  echo "$s"
}

# current <target> <app>: version the current symlink points to ("none" if missing).
current() {
  v=$(dc exec -T "$1" readlink "/home/deploy/apps/$2/current" 2>/dev/null) || { echo none; return; }
  basename "$v"
}

# currents <app> <target>...: space-separated versions of the targets.
currents() { app=$1; shift; out=""; for h in "$@"; do out="$out $(current "$h" "$app")"; done; echo "${out# }"; }

# healthy <target> <app>: 0 if /healthz of the active release answers 200.
healthy() { dc exec -T "$1" busybox wget -q -O /dev/null "http://127.0.0.1:8080/$2/current/healthz" 2>/dev/null; }

# fault <target> <app> fail|sleep <s>|clear: simulated faults read by the post_command.
fault() {
  f=/home/deploy/fault/$2
  case "$3" in
    fail) dc exec -T -u deploy "$1" touch "$f.fail" ;;
    sleep) dc exec -T -u deploy "$1" sh -c "echo $4 > $f.sleep" ;;
    clear) dc exec -T -u deploy "$1" rm -f "$f.fail" "$f.sleep" ;;
  esac
}

# runs <target> <app>: "start" lines of the post_command (deploy runs on the target).
runs() { dc exec -T "$1" sh -c "cat /home/deploy/fault/$2.log 2>/dev/null | grep -c start || true"; }

# steps <id>: names of the run steps, space separated.
step_names() { manent status "$1" | jq -r '[.result.steps[]?.name] | join(" ")'; }

# vm_version <vm> <container>: version (manent.version label) of the container ("none" if missing).
vm_version() {
  v=$(dcp exec -T "$1" docker inspect -f '{{index .Config.Labels "manent.version"}}' "$2" 2>/dev/null) || { echo none; return; }
  echo "$v"
}

# vm_image <vm> <container>: digest of the container image.
vm_image() { dcp exec -T "$1" docker inspect -f '{{.Config.Image}}' "$2" 2>/dev/null | sed 's/.*@sha256://'; }

# vm_get <vm> <port> <path>: the application's answer on the VM ("" if it does not answer).
vm_get() { dcp exec -T "$1" wget -q -O - "http://127.0.0.1:$2$3" 2>/dev/null | tr -d '\r\n'; }

# kube <arguments>: kubectl in the lab's k3s cluster.
kube() { dcp exec -T k3s kubectl "$@"; }

# k8s_manifest <app> <version> <digest> [extra]: writes the <app>-<version>.yaml manifests to
# .work/artifacts (Namespace, Deployment with readinessProbe, Service; with "extra" also one
# more ConfigMap) and prints their sha256. The image is pinned to the digest; namespace and
# replicas come from the environment configuration (${MANENT_NAMESPACE}, var.REPLICAS).
k8s_manifest() {
  f=$WORK/artifacts/$1-$2.yaml
  cat >"$f" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: \${MANENT_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $1
spec:
  replicas: \${REPLICAS}
  progressDeadlineSeconds: 25
  selector:
    matchLabels: {app: $1}
  template:
    metadata:
      labels: {app: $1}
    spec:
      containers:
        - name: web
          image: ${K8S_IMAGE:-registry:5000/uc/api@sha256:$3}
          ports: [{containerPort: 8080}]
          readinessProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 2
            failureThreshold: 2
---
apiVersion: v1
kind: Service
metadata:
  name: $1
spec:
  selector: {app: $1}
  ports: [{port: 80, targetPort: 8080}]
EOF
  if [ "${4:-}" = extra ]; then
    printf -- '---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s-extra\ndata:\n  version: "%s"\n' "$1" "$2" >>"$f"
  fi
  sha "$f"
}

# k8s_image <namespace> <deployment>: digest of the Deployment image.
k8s_image() { kube -n "$1" get deploy "$2" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null | sed 's/.*@sha256://'; }

# k8s_ready <namespace> <deployment>: ready replicas.
k8s_ready() { kube -n "$1" get deploy "$2" -o jsonpath='{.status.readyReplicas}' 2>/dev/null; }

# chart <app> <version> <digest>: packages the Helm chart <app> (Deployment and Service on port
# 80, image uc/<app> pinned to the digest, readinessProbe on /healthz) as .work/artifacts/<app>-<version>.tgz,
# like "helm package", and prints its sha256.
chart() {
  d=$(mktemp -d)
  mkdir -p "$d/$1/templates"
  printf 'apiVersion: v2\nname: %s\nversion: %s\nappVersion: "%s"\n' "$1" "$2" "$2" >"$d/$1/Chart.yaml"
  printf 'image: registry:5000/uc/%s@sha256:%s\nreplicas: 2\n' "$1" "$3" >"$d/$1/values.yaml"
  cat >"$d/$1/templates/web.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Chart.Name }}
  labels: {app: {{ .Chart.Name }}}
spec:
  replicas: {{ .Values.replicas }}
  progressDeadlineSeconds: 25
  selector:
    matchLabels: {app: {{ .Chart.Name }}}
  template:
    metadata:
      labels: {app: {{ .Chart.Name }}}
    spec:
      containers:
        - name: web
          image: {{ .Values.image }}
          ports: [{containerPort: 8080}]
          readinessProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 2
            failureThreshold: 2
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Chart.Name }}
spec:
  selector: {app: {{ .Chart.Name }}}
  ports: [{port: 80, targetPort: 8080}]
YAML
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/$1-$2.tgz" -C "$d" "$1"
  rm -rf "$d"
  sha "$WORK/artifacts/$1-$2.tgz"
}

# k3s_up: starts registry and k3s and copies the cluster CA where the workers read it.
k3s_up() {
  dcp up -d registry k3s >/dev/null 2>&1
  wait_registry || return 1
  t=0
  until kube -n default get serviceaccount default >/dev/null 2>&1; do t=$((t + 1)); [ "$t" -gt 120 ] && return 1; sleep 1; done
  dcp exec -T k3s cat /var/lib/rancher/k3s/server/tls/server-ca.crt >"$WORK/k3s/ca.crt" 2>/dev/null
  [ -s "$WORK/k3s/ca.crt" ]
}

# war <app> <version> [broken|invalid]: creates .work/artifacts/<app>-<version>.war (static
# files version and healthz) and prints its sha256. "broken" has no healthz (post-test red),
# "invalid" has a malformed WEB-INF/web.xml (WildFly refuses to deploy it).
war() {
  python3 - "$WORK/artifacts/$1-$2.war" "$2" "${3:-}" <<'PY'
import sys, zipfile
path, version, kind = sys.argv[1:4]
with zipfile.ZipFile(path, "w") as z:
    z.writestr("version", version + "\n")
    if kind != "broken":
        z.writestr("healthz", "ok " + version + "\n")
    if kind == "invalid":
        z.writestr("WEB-INF/web.xml", "<web-app><servlet>")
PY
  sha "$WORK/artifacts/$1-$2.war"
}
