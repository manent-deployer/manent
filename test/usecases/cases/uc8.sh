# UC8: interfaccia web. manent-server serve la UI incorporata; il login passa dall'identity
# provider di prova (oidc, utenti ci, cab, admin, viewer). Controlli HTTP sulla UI e sui nuovi
# endpoint, poi i test di Playwright (web/e2e) nel container ufficiale, sulla rete del laboratorio:
# login, navigazione e accessibilità (axe), dettaglio con log in tempo reale, promozione con
# dry-run, divieto di auto-approvazione, approvazione da un altro utente, tastiera e lingua.
APP=uc8-web
PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.49.1-noble
log "UC8 · interfaccia web"

info "-- UI e configurazione pubblica"
headers=$(curl -fsS -D - -o "$WORK/ui.html" http://127.0.0.1:18080/deployments)
expect_match "la UI è servita anche sui percorsi dell'app" "$(cat "$WORK/ui.html")" '<div id="root">'
expect_match "Content-Security-Policy senza script inline" "$headers" "script-src 'self';"
expect_match "la pagina non è incorporabile" "$headers" "frame-ancestors 'none'"
expect_match "connect-src ammette l'issuer OIDC" "$headers" "connect-src 'self' http://oidc:9000"
cfg=$(curl -fsS http://127.0.0.1:18080/ui/config.json)
expect_eq "config.json: login OIDC con il client della UI" "$(printf '%s' "$cfg" | jq -r '"\(.auth_mode) \(.client_id)"')" "oidc manent-ui"
expect_eq "un percorso API sconosciuto resta un 404 JSON" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer uc-viewer-token' http://127.0.0.1:18080/api/v1/inesistente)" 404

info "-- nuovi endpoint dell'API"
expect_eq "whoami: identità e ruolo dal token" "$(manent whoami | jq -r '"\(.subject) \(.grants[0].role)"')" "token:ci operator"
expect_eq "il viewer non ha approvazioni da dare" "$(as viewer manent approvals | jq length)" 0

artifact $APP 1.0.0 >/dev/null
id=$(submit "$(descriptor uc8-a uc8-100 $APP test 1.0.0)")
expect_eq "deploy di partenza in test" "$(wait_state "$id" terminal 90)" SUCCESS
page1=$(curl -fsS -H 'Authorization: Bearer uc-viewer-token' 'http://127.0.0.1:18080/api/v1/deployments?limit=1')
cursor=$(printf '%s' "$page1" | jq -r .next_cursor)
page2=$(curl -fsS -H 'Authorization: Bearer uc-viewer-token' "http://127.0.0.1:18080/api/v1/deployments?limit=1&cursor=$cursor")
[ "$cursor" != null ] && [ "$(printf '%s' "$page2" | jq -r '.deployments[0].id')" != "$cursor" ] &&
  pass "paginazione con cursore" || fail "paginazione con cursore" "$page1 / $page2"
events=$(curl -fsS -N --max-time 20 -H 'Authorization: Bearer uc-viewer-token' "http://127.0.0.1:18080/api/v1/deployments/$id/events")
expect_match "stream SSE: stato, log e fine" "$(printf '%s' "$events" | tr '\n' ' ')" "event: status .*event: log .*event: end"

info "-- Playwright (web/e2e)"
if ! docker image inspect "$PLAYWRIGHT_IMAGE" >/dev/null 2>&1; then
  docker pull -q "$PLAYWRIGHT_IMAGE" >/dev/null 2>&1 || fail "immagine di Playwright non disponibile"
fi
repo=$(cd "$UC_DIR/../.." && pwd)
rm -rf "$repo/web/test-results"
docker run --rm --network manent-uc_core --ipc=host -v "$repo":/repo -w /repo/web \
  -e DEP_ID="$id" "$PLAYWRIGHT_IMAGE" npx playwright test >"$WORK/playwright.log" 2>&1
results="$repo/web/test-results/results.json"
if [ -s "$results" ]; then
  # Una verifica della suite per ogni test di Playwright, con l'errore se fallisce.
  jq -r '.. | objects | select(has("specs")) | .specs[] | "\(.tests[0].results[-1].status)\t\(.title)\t\(.tests[0].results[-1].error.message // "" | gsub("\n"; " ") | .[0:300])"' "$results" |
    while IFS="$(printf '\t')" read -r status title err; do
      if [ "$status" = passed ]; then printf 'P\t%s\n' "$title"; else printf 'F\t%s\t%s (%s)\n' "$title" "$err" "$status"; fi
    done >"$WORK/playwright.tsv"
  while IFS="$(printf '\t')" read -r r title err; do
    if [ "$r" = P ]; then pass "UI: $title"; else fail "UI: $title" "$err"; fi
  done <"$WORK/playwright.tsv"
else
  fail "Playwright non ha prodotto risultati" "$(tail -20 "$WORK/playwright.log")"
fi
