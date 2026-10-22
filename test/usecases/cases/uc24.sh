# UC24: tenants as the teams that own the applications. payments owns uc24-shop (critical) and
# uc24-billing, retail owns another uc24-shop, legacy owns uc24-legacy (2025 contract); uc24-tools
# is listed by no tenant. The release identity is tenant/application/artifact/environment: the
# tenant comes from the registry (chosen with --tenant when the name is in several tenants), with
# the team's queue and restrictions; deploy IDs, dependencies and releases do not cross tenants;
# the members of a team get its implicit role; qualified application patterns; permissions,
# freezes and listings by application tags; a Rego policy by tenant tag.
log "UC24 · tenant proprietari: applicazioni omonime, untenanted, coda e limiti del team, ruoli impliciti, selettori, policy per contratto"

for a in uc24-shop uc24-billing uc24-legacy uc24-tools; do
  for v in 1.0.0 1.1.0; do artifact $a $v >/dev/null; done
done
pay="tenant: payments"

info "-- registro dei tenant"
expect_eq "uc24-shop è in due tenant" "$(manent tenants list -a uc24-shop | jq -r '[.[].name] | join(",")')" "payments,retail"
expect_eq "applicazioni di payments con i loro tag" \
  "$(manent tenants show payments | jq -r '[.applications | to_entries[] | .key + "=" + .value.tags.criticality] | join(",")')" "uc24-billing=medium,uc24-shop=high"
expect_eq "coda del team" "$(manent tenants show payments | jq -r .queue)" deployments.payments

info "-- il tenant viene dal registro: nomi ambigui, scelta con tenant, untenanted"
expect_match "uc24-shop senza tenant è ambiguo" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-a0 uc24-100-x uc24-shop prod 1.0.0)")" "belongs to the tenants payments, retail"
expect_match "un tenant che non elenca l'applicazione è rifiutato" \
  "$(manent_err deploy --no-wait --tenant legacy -f "$(descriptor uc24-a0 uc24-100-x uc24-shop prod 1.0.0)")" "tenant legacy does not list application uc24-shop"
id=$(submit "$(descriptor uc24-a uc24-100-shop uc24-shop prod 1.0.0 "$pay")")
expect_eq "deploy di payments/uc24-shop riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
st=$(manent status "$id")
expect_eq "il deploy è del tenant payments, sulla sua coda" "$(printf '%s' "$st" | jq -r '.tenant + " " + .queue')" "payments deployments.payments"
expect_eq "lo step config riporta il tenant" "$(printf '%s' "$st" | jq -r '.result.steps[] | select(.name=="config") | .output.tenant')" payments
expect_eq "payments/uc24-shop sul suo target" "$(current eu1-a uc24-shop)" 1.0.0
id=$(manent deploy --no-wait --tenant retail -f "$(descriptor uc24-a2 uc24-200-shop uc24-shop prod 1.1.0)" | jq -r .id)
expect_eq "deploy di retail/uc24-shop riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "con la configurazione di retail (eu1-c, sua directory)" \
  "$(dc exec -T eu1-c readlink /home/deploy/apps/retail-uc24-shop/current 2>/dev/null | xargs basename 2>/dev/null)" 1.1.0
expect_eq "due release distinte nell'inventario" \
  "$(manent inventory -a uc24-shop | jq -r '[.[] | .tenant + "=" + .current.version] | sort | join(",")')" "payments=1.0.0,retail=1.1.0"
id=$(submit "$(descriptor uc24-b uc24-100-legacy uc24-legacy prod 1.0.0)")
expect_eq "deploy di uc24-legacy riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "tenant legacy, coda di default" "$(manent status "$id" | jq -r '.tenant + " " + .queue')" "legacy deployments"
id=$(submit "$(descriptor uc24-c uc24-100-tools uc24-tools prod 1.0.0)")
expect_eq "un'applicazione che nessun tenant elenca si rilascia" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "come tenant untenanted, sulla coda di default" "$(manent status "$id" | jq -r '.tenant + " " + .queue')" "untenanted deployments"
expect_match "il rollback di un nome ambiguo chiede il tenant" \
  "$(manent_err rollback --no-wait -a uc24-shop -e prod)" "choose one with tenant"

info "-- limiti del tenant"
extra='tenant: payments
secrets: [{name: extra, ref: "env:HOME"}]'
expect_match "un segreto fuori dai pattern è rifiutato" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-d uc24-101 uc24-shop prod 1.0.0 "$extra")")" "not allowed"
expect_match "una sorgente fuori dai prefissi del team è rifiutata" \
  "$(manent_err deploy --no-wait -f "$(art_descriptor uc24-e uc24-102 uc24-shop prod 1.0.0 application file:///srv/artifacts/uc1-shop-1.0.0.tar.gz "$pay")")" "artifact_sources"

info "-- ID, dipendenze e release non attraversano i tenant"
expect_match "un ID usato da un altro tenant è rifiutato" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-f uc24-100-legacy uc24-shop prod 1.1.0 "$pay")")" "already in use"
expect_match "after verso il deploy di un altro tenant è rifiutato" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-g uc24-103 uc24-shop prod 1.1.0 "$pay
after: [uc24-100-legacy]")")" "another tenant"
descriptor uc24-r-shop uc24-r-shop uc24-shop prod 1.1.0 "$pay" >/dev/null
descriptor uc24-r-legacy uc24-r-legacy uc24-legacy prod 1.1.0 >/dev/null
cat >"$WORK/uc/uc24-r.yml" <<'EOR'
apiVersion: manent/v1
kind: Release
id: uc24-r
deploys:
  - {name: shop, file: uc24-r-shop.yml}
  - {name: legacy, file: uc24-r-legacy.yml}
EOR
expect_match "una release con applicazioni di due tenant è rifiutata" "$(manent_err release submit -f /uc/uc24-r.yml)" "one tenant"

info "-- ruolo implicito dei membri del team"
id=$(as supacme submit "$(descriptor uc24-h uc24-100-billing uc24-billing prod 1.0.0)")
expect_eq "un membro di team-payments rilascia uc24-billing" "$(wait_state "$id" terminal 60)" SUCCESS
expect_match "ma non uc24-legacy" \
  "$(as supacme manent_err deploy --no-wait -f "$(descriptor uc24-i uc24-104 uc24-legacy prod 1.1.0)")" "cannot perform deployment.submit on legacy/uc24-legacy/prod"
expect_match "né l'omonimo uc24-shop di retail, indicato per errore" \
  "$(as supacme manent_err deploy --no-wait --tenant retail -f "$(descriptor uc24-i2 uc24-105 uc24-shop prod 1.1.0)")" "cannot perform deployment.submit on retail/uc24-shop/prod"
expect_match "né le applicazioni untenanted" \
  "$(as supacme manent_err deploy --no-wait -f "$(descriptor uc24-i3 uc24-106 uc24-tools prod 1.1.0)")" "cannot perform deployment.submit on uc24-tools/prod"
expect_eq "il membro vede solo il suo tenant" "$(as supacme manent tenants list | jq -r '[.[].name] | join(",")')" payments
expect_eq "e legge l'inventario degli altri team (cross_tenant_inventory)" \
  "$(as supacme manent inventory -a uc24-legacy | jq -r '.[0].current.version')" 1.0.0

info "-- selettori sui tag delle applicazioni"
expect_eq "inventario per selettore" "$(manent inventory --selector tenant=payments | jq -r '[.[].application] | sort | join(",")')" "uc24-billing,uc24-shop"
expect_eq "elenco dei deploy per selettore" "$(manent list --selector criticality=high | jq -r '[.[] | .tenant + "/" + .application] | unique | join(",")')" payments/uc24-shop
sim=$(as pol manent policy access current --subject oidc:sam --group sre --action deployment.approve -t payments -a uc24-shop -e prod --json)
expect_eq "gli SRE approvano le applicazioni critiche (binding con selettore)" "$(printf '%s' "$sim" | jq -r .check.allowed)" true
sim=$(as pol manent policy access current --subject oidc:sam --group sre --action deployment.approve -t retail -a uc24-shop -e prod --json)
expect_eq "ma non l'omonimo di retail, che non è critico" "$(printf '%s' "$sim" | jq -r .check.allowed)" false
as admin manent freeze add --name uc24-critical --reason "chiusura delle applicazioni critiche" -a '*/uc24-*' -e prod --selector criticality=high --for 1h >/dev/null
expect_match "il freeze per selettore ferma payments/uc24-shop (critica)" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-j uc24-107 uc24-shop prod 1.1.0 "$pay")")" 'freeze "uc24-critical"'
id=$(submit "$(descriptor uc24-k uc24-110-billing uc24-billing prod 1.1.0)")
expect_eq "uc24-billing non è toccata dal freeze" "$(wait_state "$id" terminal 60)" SUCCESS
as admin manent freeze delete uc24-critical >/dev/null 2>&1

info "-- spostamento di un'applicazione tra tenant (onboarding)"
rep=$(dc exec -T api manent-server admin move-application -a uc24-tools --from untenanted --to retail --dry-run 2>/dev/null)
expect_eq "lo storico di uc24-tools passerebbe a retail" "$(printf '%s' "$rep" | jq -r '"\(.renamed) \(.dry_run)"')" "1 true"

info "-- policy per contratto (tag del tenant)"
rm -rf "$WORK/uc/uc24-policy" && cp -R cases/uc24-policy "$WORK/uc/uc24-policy"
rev=$(as pol manent policy push -d /uc/uc24-policy -m "uc24: contratti 2025 fermi alla 1.0" | jq -r .revision)
as admin manent policy approve "$rev" >/dev/null 2>&1
sleep 3
expect_match "la revisione per contratto è in vigore" "$(as admin manent policy list | jq -r .current)" "^$rev:"
expect_match "uc24-legacy (contratto 2025) resta sulla 1.0" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc24-l uc24-110-legacy uc24-legacy prod 1.1.0)")" "2025 contract"
id=$(submit "$(descriptor uc24-m uc24-110-shop uc24-shop prod 1.1.0 "$pay")")
expect_eq "payments/uc24-shop (contratto 2026) passa alla 1.1" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "uc24-shop sulla 1.1.0" "$(current eu1-a uc24-shop)" 1.1.0

info "-- ritorno alla policy predefinita"
rm -rf "$WORK/uc/uc24-default" && mkdir -p "$WORK/uc/uc24-default" && printf '{}\n' >"$WORK/uc/uc24-default/data.json"
back=$(as pol manent policy push -d /uc/uc24-default -m "ritorno alle regole di manent.yml" | jq -r .revision)
as admin manent policy approve "$back" >/dev/null 2>&1
sleep 3
expect_match "policy predefinita di nuovo in vigore" "$(as admin manent policy list | jq -r .current)" "^$back:"
