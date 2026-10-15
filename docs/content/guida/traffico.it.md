# Rilasci per percentuale

Con la strategia `traffic` la nuova versione, la **candidata**, viene rilasciata accanto a quella in esercizio, la **stabile**, senza ricevere traffico. Poi manent sposta il traffico per gradini (per esempio 1, 5, 25, 50, 100%), con un tempo di osservazione e un health gate dopo ogni gradino. Se qualcosa va storto il traffico torna alla stabile con un cambio di peso, senza rifare il deploy.

```yaml title="envconfig/shop/prod.yml (o descrittore)"
rollout:
  strategy: traffic
  traffic:
    steps: [1, 5, 25, 50, 100]
    bake: 10m                        # dopo ogni gradino tranne l'ultimo
    testers: {header: "x-canary: always"}
    scaleDownDelay: 30m              # la stabile resta pronta per un ritorno veloce
  approvalsBetween: ["25"]           # pausa dopo il gradino del 25%
  onFailure: rollback-all            # default per questa strategia
  health:
    metrics:
      - name: errori
        compare: stable              # candidata contro stabile
        query: 'sum(rate(http_requests_total{app="{application}",track="{track}",code=~"5.."}[5m])) / sum(rate(http_requests_total{app="{application}",track="{track}"}[5m]))'
        maxRatio: 1.5
        samplesQuery: 'sum(increase(http_requests_total{app="{application}",track="{track}"}[5m]))'
        minSamples: 200
```

**Blue/green** è lo stesso meccanismo con un solo gradino: `steps: [100]`, di solito con i tester e un'approvazione prima dello switch (`approvalsBetween: ["0"]`).

## Sequenza

1. Step iniziali (`secrets`, `artifacts`, `qa`, …).
2. **`traffic:prepare`** legge, senza modificare nulla, la versione stabile e lo slot libero.
3. **`deploy`** rilascia la candidata nello slot libero, **senza traffico**; poi gli step fino ad `activation` esclusa (`post-test`, step personalizzati).
4. **`traffic:testers`** (con `testers`) instrada verso la candidata le sole richieste dei tester.
5. Approvazione `"0"`, se richiesta: dopo i test a peso 0, prima di spostare traffico.
6. Per ogni gradino: **`traffic@<percentuale>`** imposta i pesi, poi bake e health gate (`gate@traffic@<percentuale>#n`) e, se richiesta, un'approvazione.
7. **`traffic:promote`**: la candidata diventa la versione in esercizio; poi `activation` e gli step finali.
8. Dopo `scaleDownDelay` un workflow separato (`manent.traffic-retire`) ridimensiona la versione sostituita, senza occupare la coda dell'ambiente. Se nel frattempo un altro rilascio ha cambiato la versione in esercizio, non tocca nulla.

Al **primo rilascio** non c'è una versione stabile: niente tester né gradini intermedi, la candidata riceve subito tutto il traffico.

## Errori e ritorno

Se un gradino, un gate o uno step fallisce, con `onFailure` `rollback-wave` o `rollback-all` (default per questa strategia) **`traffic:revert`** riporta tutto il traffico alla stabile e ridimensiona la candidata; se era già stata promossa, anche lo storico torna alla stabile. Non serve il rollback del plugin né `rollback.enabled`. Con `onFailure: stop` i pesi restano dove sono, per un intervento manuale.

**Deriva dei pesi.** Prima di ogni gradino manent rilegge i pesi: se non sono quelli che ha impostato (un operatore o un altro strumento li ha cambiati), il rollout si ferma (`paused-drift`) e attende una decisione con `manent approve` (prosegue sovrascrivendo i pesi) o `manent reject` (si ferma e torna alla stabile). Con `onDrift: ignore` il controllo è disattivato.

## Gate comparativi

Al 1% una soglia assoluta su errori o latenza dice poco. Una metrica con `compare: stable` è valutata due volte nello stesso istante, per la candidata e per la stabile, sostituendo `{track}` con il loro slot (`{version}` è la versione candidata):

- `maxRatio`: rapporto massimo candidata/stabile;
- `maxDelta`: differenza massima candidata − stabile;
- `samplesQuery` e `minSamples`: sotto il minimo di richieste osservate il controllo non è verde e il gradino fallisce alla fine del bake. Un gate che non vede traffico non può dire che il rilascio è sano.

Le metriche dell'applicazione devono avere l'etichetta dello slot: su Kubernetes è `manent.io/track` sui pod, da portare in Prometheus con il relabeling.

## Router

| Router | Slot | Come si sposta il traffico | Tester |
|---|---|---|---|
| [kubernetes](../plugins/kubernetes.md#rilasci-per-percentuale) con Gateway API | oggetti con suffisso `-a`/`-b` | pesi dei `backendRefs` `<servizio>-a` e `<servizio>-b` di una `HTTPRoute` | regola della route con un match sull'header |
| [kubernetes](../plugins/kubernetes.md#rilasci-per-percentuale) con Istio | workload con suffisso, Service comune | pesi delle route verso i subset `a` e `b` di un `VirtualService` | voce `http` con un match sull'header |
| [lambda](../plugins/lambda.md#rilasci-per-percentuale) | versioni pubblicate | alias con un peso aggiuntivo (`AdditionalVersionWeights`) | invocazione diretta della versione candidata |
| [cloudrun](../plugins/cloudrun.md#rilasci-per-percentuale) | revisioni | traffico del servizio diviso tra due revisioni | URL del tag della revisione (`testers.tag`) |

Su Lambda e Cloud Run le versioni senza traffico non consumano risorse: un blue/green non raddoppia la capacità. Anche un [plugin esterno](../plugins/esterni.md) può fare da router, dichiarando le capacità `slots` e `traffic_router`.

### Router dell'ambiente e slot sui target

Con plugin che aggiornano i target in place (`remote-copy`, `container`, `ecs` con due servizi), gli slot sono **gruppi di target**: ogni target ha l'etichetta `slot: a` o `slot: b`, il deploy aggiorna solo i target dello slot libero e un router della configurazione di ambiente sposta il traffico tra i due gruppi.

```yaml title="envconfig/shop/prod.yml"
targets:
  - {host: 10.0.1.10, labels: {slot: a}}
  - {host: 10.0.2.10, labels: {slot: b}}
traffic:
  router:
    provider: aws-alb
    settings:
      region: eu-west-1
      forward_arn: arn:aws:elasticloadbalancing:eu-west-1:123456789012:listener/app/shop/…
      target_group_a: arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/shop-a/…
      target_group_b: arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/shop-b/…
      tester_rule_arn: arn:aws:elasticloadbalancing:eu-west-1:123456789012:listener-rule/app/shop/…
```

Con i router dei load balancer (`haproxy`, `f5`, `nginx-plus`) i due slot sono server dello stesso backend: la quota di uno slot è divisa tra i suoi server, e l'impostazione `member` (default `{host}`, anche `{host}:{port}` o `{label.<nome>}`) dà il nome del server di un target. Lo slot stabile è quello con il peso maggiore nel router; il primo rilascio va nello slot `a`. Il ritiro non ridimensiona nulla: lo slot sostituito resta pronto per il rilascio successivo.

| Router | Impostazioni |
|---|---|
| `haproxy` | Come il [pool haproxy](svuotamento.md#haproxy); i pesi (0-256) dei server del backend cambiano con l'API di configurazione della Data Plane API su ogni istanza. |
| `f5` | Come il [pool f5](svuotamento.md#f5); il pool deve usare un bilanciamento *ratio (member)*: manent cambia il ratio dei membri e disabilita quelli con peso 0. |
| `nginx-plus` | Come il [pool nginx-plus](svuotamento.md#nginx-plus); manent cambia il peso dei server dell'upstream e mette in drain quelli con peso 0. |
| `aws-alb` | `region`, `forward_arn` (listener o regola con l'azione forward), `target_group_a`, `target_group_b`, `tester_rule_arn` facoltativa (regola, ad esempio su un header, che porta i tester alla candidata), credenziali come per `aws-elb`. Permessi: `elasticloadbalancing:Describe*`, `ModifyListener` o `ModifyRule`. |

Per i servizi ECS con una strategia nativa (blue/green, lineare, canary) c'è anche la [modalità delegata](../plugins/ecs.md#strategie-native): ECS sposta il traffico da solo e manent ne attende la fine.

**Convivenza con GitOps.** Se la route è gestita anche da Argo CD o Flux, i due strumenti si contendono i pesi: manent cambia solo le regole della route (con una patch condizionata al `resourceVersion` letto), e va configurato `ignoreDifferences` (Argo CD) o l'equivalente sui pesi.

## Stato

`manent status`, `manent activity` e l'interfaccia web mostrano gli stadi (un gradino ciascuno), il router, gli slot stabile e candidato, la quota corrente della candidata, se i tester sono instradati e se la candidata è stata promossa. Gli eventi `step` dei [callback](callback.md) includono `traffic:*` e `traffic@<percentuale>`.

## Riferimento

| `rollout.traffic` | Default | Descrizione |
|---|---|---|
| `steps` | | Percentuali crescenti, l'ultima 100 (al più 20 gradini). |
| `bake` | `0` | Osservazione dopo ogni gradino tranne l'ultimo. |
| `testers.header` | | `"nome: valore"` per i router che instradano per header. |
| `testers.tag` | | Tag della revisione candidata (Cloud Run). |
| `scaleDownDelay` | `30m` | Attesa prima di ridimensionare la versione sostituita. |
| `onDrift` | `pause` | `pause` o `ignore`. |
| `replicas` | | `proportional`: la candidata parte con una replica e cresce prima di ogni gradino fino alle repliche dei manifest (plugin che implementano `plugin.SlotScaler`, come `kubernetes`). |

`approvalsBetween` con questa strategia accetta `"0"`, le percentuali dei gradini tranne l'ultimo e, con le ondate, i nomi delle ondate.

## Su più cluster o region { #ondate }

Con `rollout.waves` il rilascio per percentuale procede un passo alla volta: per esempio prima il cluster europeo, poi gli altri. Ogni passo (un cluster o una region, secondo `select` e `by` delle [ondate](ondate.md)) esegue l'intera sequenza (preparazione, candidata, tester, gradini, promozione) prima del successivo, e un'ondata elencata in `approvalsBetween` mette in pausa il rilascio prima di passare all'ondata seguente. Gli step hanno il nome del passo (`traffic@25@eu`, `deploy@eu`), gli stadi pure (`eu/25%`), e i freeze per region valgono come nelle ondate. In caso di errore `rollback-wave` riporta alla versione stabile solo il passo fallito, `rollback-all` anche quelli già promossi, in ordine inverso.

```yaml
rollout:
  strategy: traffic
  waves:
    - {name: eu, select: "region=eu-1"}
    - {name: resto, select: "region!=eu-1", by: region}
  traffic: {steps: [5, 25, 100], bake: 15m}
  approvalsBetween: ["eu"]
```

Le ondate richiedono un router nel plugin (un passo per cluster o per region): un router dell'ambiente (`traffic.router`) sposta il traffico di tutto l'ambiente e non è ammesso con le ondate. I passi girano sul worker che esegue il deploy, anche con il routing per region.
