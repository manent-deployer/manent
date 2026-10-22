# UC27: environment configuration from a Git repository (envconfig.provider: git). A second
# manent server reads uc27-web/uc27-web/prod.yml from the lab Git server with a token: every
# deploy records the commit SHA; a new commit on main reaches the next deploy; an unsigned commit
# is refused; config.ref (--config-ref) chooses a tag, a branch outside allowed_refs or an
# unreachable SHA is refused; the manual rollback goes back to the SHA recorded by the deploy it
# restores, not to the head of main.
APP=uc27-web
log "UC27 · configurazione di ambiente da Git: SHA registrato, commit firmati, config.ref, rollback alla revisione registrata"

# mg <args>: the client towards the manent server with the configuration from Git.
mg() { dc run --rm -T -e MANENT_SERVER=http://manent-git:8080 client "$@" 2>>"$WORK/client.log"; }
mg_err() { dc run --rm -T -e MANENT_SERVER=http://manent-git:8080 client "$@" 2>&1 >/dev/null | grep -v -e '^ *Container' -e '^time='; }
# mg_wait <id>: waits for the end of a deploy and prints its state.
mg_wait() {
  t=0 s=""
  while [ "$t" -lt 90 ]; do
    s=$(mg status "$1" | jq -r .state)
    case "$s" in SUCCESS|ERROR|CANCELLED) break ;; esac
    sleep 2; t=$((t + 2))
  done
  echo "$s"
}
# gexec <script>: runs a script in the working copy of the Git server.
gexec() { dc exec -T gitserver sh -euc "cd /work && $1"; }
# config <targets...>: writes the configuration of uc27-web in prod with these targets.
config() {
  mkdir -p "$WORK/uc/uc27-repo/$APP/$APP"
  {
    echo "# UC27: configuration of uc27-web in prod, versioned in Git."
    echo "plugin: remote-copy"
    echo "targets:"
    for h in "$@"; do echo "  - {host: $h, user: deploy, labels: {region: eu-1}}"; done
    cat <<'EOF'
params:
  dest_dir: /home/deploy/apps/uc27-web
  layout: releases
  keep_releases: "5"
  ssh_key_secret: deploy-key
  prepare_command: 'tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"'
secrets:
  - {name: deploy-key, ref: file:/keys/id}
EOF
  } >"$WORK/uc/uc27-repo/$APP/$APP/prod.yml"
}
# commit <message> [unsigned]: commits the configuration, signed unless asked, and prints the SHA.
commit() {
  sign='-c gpg.format=ssh -c user.signingkey=/root/sign commit -S'
  [ "${2:-}" = unsigned ] && sign='commit'
  gexec "cp -R /uc/uc27-repo/. /work/ && git add -A && git $sign -q -m '$1' && git rev-parse HEAD"
}
d() { descriptor "uc27-$1" "$2" $APP prod "$3"; }

dc --profile git up -d gitserver manent-git >/dev/null 2>&1 || dc --profile git up -d gitserver manent-git
t=0
until curl -fs http://127.0.0.1:18082/readyz >/dev/null 2>&1; do
  t=$((t + 1)); [ "$t" -gt 60 ] && { dc --profile git logs manent-git | tail -20; break; }
  sleep 1
done
for v in 1.0.0 1.1.0 1.2.0 1.3.0; do artifact $APP $v >/dev/null; done
dc exec -T gitserver sh -euc 'install -m 600 /keys/git-sign /root/sign && git init -q --bare -b main /srv/git/envconfig.git &&
  git clone -q /srv/git/envconfig.git /work 2>/dev/null && cd /work && git checkout -q -b main'

info "-- primo deploy: il ref di default e lo SHA registrato"
config eu1-a
sha1=$(commit "uc27: eu1-a")
gexec 'git tag env-1 && git push -q origin main env-1'
id=$(mg deploy --no-wait -f "$(d a uc27-100 1.0.0)" | jq -r .id)
expect_eq "il deploy legge la configurazione da Git" "$(mg_wait "$id")" SUCCESS
st=$(mg status "$id")
expect_eq "lo stato riporta lo SHA della configurazione" "$(printf '%s' "$st" | jq -r .config_revision)" "$sha1"
expect_eq "lo step config riporta ref e sorgente" \
  "$(printf '%s' "$st" | jq -r '.result.steps[] | select(.name == "config") | "\(.output.ref) \(.output.source)"')" \
  "refs/heads/main git:http://gitserver:8080/envconfig.git//uc27-web/uc27-web/prod.yml"
expect_eq "eu1-a ha la 1.0.0" "$(current eu1-a $APP)" 1.0.0

info "-- un nuovo commit su main arriva al deploy successivo"
config eu1-a eu1-b
sha2=$(commit "uc27: eu1-a, eu1-b")
gexec 'git push -q origin main'
id=$(mg deploy --no-wait -f "$(d b uc27-110 1.1.0)" | jq -r .id)
expect_eq "il deploy riesce" "$(mg_wait "$id")" SUCCESS
expect_eq "con il nuovo SHA" "$(mg status "$id" | jq -r .config_revision)" "$sha2"
expect_eq "e il nuovo target" "$(currents $APP eu1-a eu1-b)" "1.1.0 1.1.0"

info "-- un commit non firmato è rifiutato"
config eu1-a eu1-b eu1-c
commit "uc27: non firmato" unsigned >/dev/null
gexec 'git push -q origin main'
sleep 2
err=$(mg_err deploy --no-wait -f "$(d c uc27-120 1.2.0)")
expect_match "deploy rifiutato all'invio: commit senza firma" "$err" "is not signed"
expect_eq "eu1-c non è toccato" "$(current eu1-c $APP)" none
gexec 'git reset -q --hard HEAD~1 && git push -q -f origin main'
sleep 2

info "-- config.ref: un tag ammesso, un branch fuori da allowed_refs, uno SHA non raggiungibile"
id=$(mg deploy --no-wait --config-ref env-1 -f "$(d d uc27-120-tag 1.2.0)" | jq -r .id)
expect_eq "il deploy con --config-ref env-1 riesce" "$(mg_wait "$id")" SUCCESS
expect_eq "con lo SHA del tag" "$(mg status "$id" | jq -r .config_revision)" "$sha1"
expect_eq "solo eu1-a, come nella configurazione del tag" "$(currents $APP eu1-a eu1-b)" "1.2.0 1.1.0"
gexec 'git checkout -q -b feature' >/dev/null
config eu1-c
feature=$(commit "uc27: branch di prova")
gexec 'git push -q origin feature && git checkout -q main'
err=$(mg_err deploy --no-wait --config-ref feature -f "$(d e uc27-130-feature 1.3.0)")
expect_match "un branch fuori da allowed_refs è rifiutato" "$err" "not allowed \\(allowed_refs\\)"
err=$(mg_err deploy --no-wait --config-ref "$feature" -f "$(d e uc27-130-sha 1.3.0)")
expect_match "uno SHA che main e i tag non raggiungono è rifiutato" "$err" "not reachable from the allowed refs"

info "-- il rollback torna alla revisione registrata, non alla testa di main"
config eu1-a eu1-b eu1-c
sha3=$(commit "uc27: eu1-a, eu1-b, eu1-c")
gexec 'git push -q origin main'
id=$(mg rollback --no-wait -a $APP -e prod | jq -r .id)
expect_eq "il rollback riesce" "$(mg_wait "$id")" SUCCESS
st=$(mg status "$id")
expect_eq "torna alla 1.1.0 con lo SHA registrato dal suo deploy" "$(printf '%s' "$st" | jq -r '"\(.version) \(.config_revision)"')" "1.1.0 $sha2"
expect_eq "i target di quella configurazione: eu1-c non è toccato" "$(currents $APP eu1-a eu1-b eu1-c)" "1.1.0 1.1.0 none"
id=$(mg deploy --no-wait -f "$(d f uc27-130 1.3.0)" | jq -r .id)
expect_eq "il deploy successivo legge di nuovo main" "$(mg_wait "$id")" SUCCESS
expect_eq "con tre target" "$(mg status "$id" | jq -r .config_revision) $(currents $APP eu1-a eu1-b eu1-c)" "$sha3 1.3.0 1.3.0 1.3.0"

dc --profile git stop -t 5 manent-git gitserver >/dev/null 2>&1
