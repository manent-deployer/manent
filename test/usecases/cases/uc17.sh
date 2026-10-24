# UC17: real state of the targets. The workers read the release in service on every target
# (reconcile in manent.yml) and the inventory reports the targets changed outside manent: a
# manual rollback, a release removed by hand, a target that cannot be reached.
APP=uc17-shop
log "UC17 · stato reale dei target: modifiche fuori da manent"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null

# uc17_entry: inventory entry of the environment.
uc17_entry() { manent inventory -a $APP -e prod | jq -c '.[0] // {}'; }

# uc17_wait <jq filter> [seconds]: waits until the filter on the inventory entry is true and
# prints the entry (the last one read, on timeout).
uc17_wait() {
  t=0
  while :; do
    e=$(uc17_entry)
    if [ "$(printf '%s' "$e" | jq -r "$1" 2>/dev/null)" = true ]; then printf '%s' "$e"; return 0; fi
    t=$((t + 3)); [ "$t" -ge "${2:-90}" ] && { printf '%s' "$e"; return 1; }
    sleep 3
  done
}

# uc17_state <entry> <host>: state of the target in the inventory entry.
uc17_state() { printf '%s' "$1" | jq -r --arg h "$2" '.targets.targets[]? | select(.host == $h) | "\(.state) \(.version // "")"' | sed 's/ $//'; }

# uc17_link <target> <release>: points current to the release by hand, as an operator would.
uc17_link() {
  dc exec -T -u deploy "$1" sh -c "cd /home/deploy/apps/$APP && ln -sfn /home/deploy/apps/$APP/releases/$2 .manual && mv -T .manual current"
}

info "-- rilascio: i target letti coincidono con l'inventario"
d=$(descriptor uc17-a uc17-100 $APP prod 1.0.0)
id1=$(submit "$d")
expect_eq "rilascio della 1.0.0 riuscito" "$(wait_state "$id1" terminal 90)" SUCCESS
d=$(descriptor uc17-b uc17-110 $APP prod 1.1.0)
id=$(submit "$d")
expect_eq "rilascio della 1.1.0 riuscito" "$(wait_state "$id" terminal 90)" SUCCESS
e=$(uc17_wait ".targets.deployment == \"$id\"")
expect_eq "la riconciliazione confronta i target con il deploy in esercizio" "$(printf '%s' "$e" | jq -r .targets.version)" 1.1.0
expect_eq "eu1-a allineato" "$(uc17_state "$e" eu1-a)" "in-sync 1.1.0"
expect_eq "eu1-b allineato" "$(uc17_state "$e" eu1-b)" "in-sync 1.1.0"
expect_eq "ambiente non fuori sincronia" "$(printf '%s' "$e" | jq -r '.out_of_sync // false')" false
expect_eq "region e AZ del target riportate" "$(printf '%s' "$e" | jq -r '.targets.targets[] | select(.host == "eu1-b") | "\(.region) \(.az)"')" "eu-1 eu-1b"

info "-- rollback a mano su eu1-b"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
uc17_link eu1-b 1.0.0
expect_eq "eu1-b punta alla 1.0.0 (modifica fuori da manent)" "$(current eu1-b $APP)" 1.0.0
e=$(uc17_wait '.out_of_sync == true')
expect_eq "l'inventario segnala l'ambiente fuori sincronia" "$(printf '%s' "$e" | jq -r .out_of_sync)" true
expect_eq "eu1-b drifted con la versione trovata" "$(uc17_state "$e" eu1-b)" "drifted 1.0.0"
expect_eq "eu1-a resta allineato" "$(uc17_state "$e" eu1-a)" "in-sync 1.1.0"
expect_eq "l'inventario resta sul deploy in esercizio" "$(printf '%s' "$e" | jq -r .current.version)" 1.1.0
expect_eq "manent inventory --out-of-sync elenca l'ambiente" \
  "$(manent inventory --out-of-sync | jq -r --arg a $APP '[.[] | select(.application == $a)] | length')" 1
logs=$(dc logs --no-color --since "$since" worker-1 worker-2 2>/dev/null | grep 'target changed outside manent' | grep 'host=eu1-b')
expect_match "il worker registra il cambio con versione attesa e trovata" "$logs" "expected=1.1.0 found=1.0.0"
m=$(curl -s http://127.0.0.1:18080/metrics | grep "^manent_targets{application=\"$APP\"")
expect_match "metrica manent_targets con il target drifted" "$m" "state=\"drifted\"} 1"

info "-- release rimossa a mano su eu1-a"
dc exec -T -u deploy eu1-a rm /home/deploy/apps/$APP/current
e=$(uc17_wait '([.targets.targets[]? | select(.host == "eu1-a" and .state == "missing")] | length) == 1')
expect_eq "eu1-a senza release: missing" "$(uc17_state "$e" eu1-a)" missing

info "-- target irraggiungibile"
dc stop eu1-a >/dev/null 2>&1
e=$(uc17_wait '([.targets.targets[]? | select(.host == "eu1-a" and .state == "unknown")] | length) == 1')
expect_eq "eu1-a spento: unknown" "$(uc17_state "$e" eu1-a)" unknown
expect_match "il dettaglio riporta l'errore di lettura" "$(printf '%s' "$e" | jq -r '.targets.targets[] | select(.host == "eu1-a") | .detail')" "."
dc start eu1-a >/dev/null 2>&1
sleep 3

info "-- ritorno in linea"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
uc17_link eu1-b 1.1.0
uc17_link eu1-a 1.1.0
e=$(uc17_wait '.out_of_sync != true and ([.targets.targets[]? | select(.state == "in-sync")] | length) == 2')
expect_eq "di nuovo allineati senza un nuovo deploy" "$(uc17_state "$e" eu1-a) / $(uc17_state "$e" eu1-b)" "in-sync 1.1.0 / in-sync 1.1.0"
logs=$(dc logs --no-color --since "$since" worker-1 worker-2 2>/dev/null | grep 'target in sync again' | grep -c 'host=eu1-b')
[ "$logs" -ge 1 ] && pass "il ritorno in linea di eu1-b è registrato" || fail "nessun log del ritorno in linea di eu1-b"

info "-- un nuovo deploy rende superata la lettura precedente"
uc17_link eu1-b 1.0.0
uc17_wait '.out_of_sync == true' >/dev/null
prev=$id
d=$(descriptor uc17-c uc17-110-redeploy $APP prod 1.1.0)
id=$(submit "$d")
expect_eq "nuovo rilascio della 1.1.0 riuscito" "$(wait_state "$id" terminal 90)" SUCCESS
# Right after the deploy there is no reading yet, or already the new one: never the old one.
e=$(uc17_entry)
expect_eq "la lettura del deploy precedente non conta più" "$(printf '%s' "$e" | jq -r --arg p "$prev" '(.targets.deployment // "") != $p and (.out_of_sync // false) == false')" true
e=$(uc17_wait ".targets.deployment == \"$id\"")
expect_eq "la lettura successiva confronta i target con il nuovo deploy" "$(uc17_state "$e" eu1-a) / $(uc17_state "$e" eu1-b)" "in-sync 1.1.0 / in-sync 1.1.0"
