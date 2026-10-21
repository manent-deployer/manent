# UC18: deploy from the CI. The GitHub Action script (integrations/github/run.sh) runs with the
# variables a runner sets (INPUT_*, GITHUB_OUTPUT, GITHUB_STEP_SUMMARY), the GitLab template job
# (integrations/gitlab/manent.yml) with its inputs substituted as GitLab would: both in a
# container with a shell and the manent client, through "manent deploy --ci".
APP=uc18-shop
log "UC18 · deploy dalla CI: GitHub Action e template GitLab"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null
artifact $APP 1.2.0 broken >/dev/null
rm -rf "$WORK/ci"/* 2>/dev/null

# The descriptor of the repository: version and source come from the pipeline.
cat >"$WORK/uc/uc18.yml" <<EOT
apiVersion: manent/v1
application: $APP
environment: prod
artifact:
  type: application
  name: $APP
  version: "0.0.0"
  source: file:///srv/artifacts/$APP-0.0.0.tar.gz
rollback: {enabled: true}
steps: {post-test: true}
postTest:
  http:
    - {url: "http://eu1-c:8080/$APP/current/healthz"}
EOT
src() { echo "file:///srv/artifacts/$APP-$1.tar.gz"; }

# gh <INPUT_...=value>...: runs the action step as a GitHub runner; prints the exit code.
gh() {
  set -- "$@"
  envs=""
  for kv in "$@"; do envs="$envs -e $kv"; done
  # shellcheck disable=SC2086
  dc run --rm -T -e GITHUB_ACTIONS=true -e GITHUB_OUTPUT=/ci/gh_output -e GITHUB_STEP_SUMMARY=/ci/gh_summary.md $envs \
    ci /integrations/github/run.sh >"$WORK/ci/gh.log" 2>&1
  echo $?
}
# out <file> <key>: last value of the key in an outputs file.
out() { grep "^$2=" "$WORK/ci/$1" 2>/dev/null | tail -1 | cut -d= -f2-; }

info "-- GitHub Action"
code=$(gh INPUT_DESCRIPTOR=/uc/uc18.yml INPUT_VERSION=1.0.0 INPUT_SOURCE="$(src 1.0.0)" INPUT_ID=gh-1001 INPUT_TIMEOUT=3m)
expect_eq "lo step dell'action riesce" "$code" 0
expect_eq "output deployment_id" "$(out gh_output deployment_id)" gh-1001
expect_eq "output state" "$(out gh_output state)" SUCCESS
expect_eq "output version (dalla pipeline, non dal descrittore)" "$(out gh_output version)" 1.0.0
expect_match "riepilogo del job: titolo" "$(cat "$WORK/ci/gh_summary.md")" "### manent · Deploy di uc18-shop in prod"
expect_match "riepilogo del job: esito" "$(cat "$WORK/ci/gh_summary.md")" "Esito \| \*\*SUCCESS\*\*"
expect_eq "eu1-c sulla 1.0.0" "$(current eu1-c $APP)" 1.0.0
expect_eq "una esecuzione sul target" "$(runs eu1-c $APP)" 1

code=$(gh INPUT_DESCRIPTOR=/uc/uc18.yml INPUT_VERSION=1.0.0 INPUT_SOURCE="$(src 1.0.0)" INPUT_ID=gh-1001)
expect_eq "job rieseguito con lo stesso id: riesce" "$code" 0
expect_eq "idempotenza: nessuna nuova esecuzione sul target" "$(runs eu1-c $APP)" 1

code=$(gh INPUT_DESCRIPTOR=/uc/uc18.yml INPUT_VERSION=1.1.0 INPUT_SOURCE="$(src 1.1.0)" INPUT_DRY_RUN=true)
expect_eq "dry-run dalla CI riuscito" "$code" 0
expect_eq "output dry_run" "$(out gh_output dry_run)" true
expect_eq "il dry-run non tocca il target" "$(current eu1-c $APP)" 1.0.0

code=$(gh INPUT_DESCRIPTOR=/uc/uc18.yml INPUT_VERSION=1.2.0 INPUT_SOURCE="$(src 1.2.0)" INPUT_ID=gh-1003)
expect_eq "release difettosa: lo step fallisce" "$([ "$code" -ne 0 ] && echo fallito || echo riuscito)" fallito
expect_eq "output state anche sul fallimento" "$(out gh_output state)" ERROR
sum=$(tail -12 "$WORK/ci/gh_summary.md")
expect_match "il riepilogo riporta il rollback" "$sum" "Rollback \| completed"
expect_match "il riepilogo riporta l'errore" "$sum" "Errore \|.*post-test"
expect_eq "eu1-c tornato alla 1.0.0" "$(current eu1-c $APP)" 1.0.0

code=$(gh INPUT_COMMAND=validate INPUT_DESCRIPTOR=/uc/uc18.yml)
expect_eq "command validate" "$code" 0
expect_match "validate mostra il deploy effettivo" "$(cat "$WORK/ci/gh.log")" '"valid": true'

cp config/envconfig/$APP/$APP/prod.yml "$WORK/uc/uc18-env-ok.yml"
sed 's/^targets:/targetz:/' config/envconfig/$APP/$APP/prod.yml >"$WORK/uc/uc18-env-bad.yml"
code=$(gh INPUT_COMMAND=env-validate INPUT_APPLICATION=$APP INPUT_ENVIRONMENT=prod INPUT_ENV_CONFIG=/uc/uc18-env-ok.yml)
expect_eq "env-validate: configurazione candidata valida" "$code" 0
expect_match "env-validate usa il deploy in esercizio come esempio" "$(cat "$WORK/ci/gh.log")" '"source": "configurazione candidata"'
code=$(gh INPUT_COMMAND=env-validate INPUT_APPLICATION=$APP INPUT_ENVIRONMENT=prod INPUT_ENV_CONFIG=/uc/uc18-env-bad.yml)
expect_eq "env-validate: configurazione con un campo sconosciuto respinta" "$([ "$code" -ne 0 ] && echo respinta || echo accettata)" respinta
expect_match "l'errore indica il campo" "$(cat "$WORK/ci/gh.log")" "targetz"
code=$(gh INPUT_COMMAND=publish)
expect_eq "command sconosciuto: errore di uso" "$code" 2

info "-- template GitLab"
# The job lines between the markers, with $[[ inputs.* ]] substituted as GitLab does.
script=$(awk '/manent:script:begin/ {f = 1; next} /manent:script:end/ {f = 0} f' ../../integrations/gitlab/manent.yml |
  sed -e '/^ *- |$/d' -e 's/^      //' \
    -e 's#\$\[\[ inputs.descriptor \]\]#/uc/uc18.yml#g' -e 's#\$\[\[ inputs.version \]\]#1.1.0#g' \
    -e "s#\\\$\\[\\[ inputs.source \\]\\]#$(src 1.1.0)#g" -e 's#\$\[\[ inputs.sha256 \]\]##g' \
    -e 's#\$\[\[ inputs.id \]\]#gl-$CI_PIPELINE_ID#g' -e 's#\$\[\[ inputs.dry_run \]\]#false#g' \
    -e 's#\$\[\[ inputs.timeout \]\]#3m#g' -e 's#\$\[\[ inputs.manent_ref \]\]#main#g')
expect_eq "nessun input lasciato senza sostituzione" "$(printf '%s' "$script" | grep -c 'inputs\.')" 0
printf '%s\n' "$script" >"$WORK/ci/gitlab-job.sh"
dc run --rm -T -e GITLAB_CI=true -e CI_PIPELINE_ID=77 ci /ci/gitlab-job.sh >"$WORK/ci/gl.log" 2>&1
expect_eq "il job GitLab riesce" "$?" 0
expect_eq "report dotenv: deployment_id con la variabile della pipeline" "$(out manent.env deployment_id)" gl-77
expect_eq "report dotenv: state" "$(out manent.env state)" SUCCESS
expect_eq "eu1-c sulla 1.1.0" "$(current eu1-c $APP)" 1.1.0
