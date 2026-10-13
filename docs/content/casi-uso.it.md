# Casi d'uso

I casi d'uso provano manent da capo a fondo, come gira in esercizio: server API, worker, client e target SSH in container separati, su Postgres. Si parte da un singolo deploy pianificato e si arriva ai rilasci a ondate su più region, poi ai container su VM e a Kubernetes; per ogni caso si verificano anche fallimenti e rollback.

```sh
make test-usecases                    # tutti i casi (circa 18 minuti)
make test-usecases CASES="uc1 uc4"    # solo alcuni
make test-usecases KEEP=1             # lascia il laboratorio attivo alla fine
```

Servono Docker con Compose v2, Go, `jq` e `ssh-keygen`. Lo script compila i binari linux sull'host, quindi non serve l'immagine `golang`. UC6 e UC7 usano container privilegiati (Docker in Docker e k3s) e circa 2 GB di memoria in più per Docker. UC8 usa l'immagine di Playwright (circa 2 GB su disco).

## Il laboratorio

```mermaid
flowchart LR
    C[client manent] --> A[API]
    A --> P[(Postgres)]
    W1[worker-1] --> P
    W2[worker-2] --> P
    subgraph eu-1
      EA[eu1-a · eu-1a]
      EB[eu1-b · eu-1b]
      EC[eu1-c · eu-1c]
    end
    subgraph us-1
      UA[us1-a · us-1a]
      UB[us1-b · us-1b]
    end
    W1 --> eu-1
    W1 --> us-1
    W2 --> eu-1
    W2 --> us-1
    subgraph uc67[UC6 · UC7]
      R[(registry)]
      VA[vm-a · Docker]
      VB[vm-b · Docker]
      K[k3s]
    end
    W1 --> uc67
    W2 --> uc67
```

- **Target:** container Debian con SSH (per il plugin `remote-copy`, layout `releases`) e un server HTTP che espone la release attiva: `http://<target>:8080/<app>/current/healthz`. Una release senza `healthz` simula una versione difettosa.
- **Configurazione di ambiente:** target, parametri e segreti arrivano dal provider `file` (`test/usecases/config/envconfig`); i descrittori contengono solo l'artefatto.
- **Accesso:** token statici con ruoli diversi (`ci` rilascia, `cab` approva, `admin` può derogare ai freeze, `viewer` legge soltanto), e un identity provider OIDC di prova (`test/usecases/oidc`) con gli stessi utenti per l'interfaccia web.
- **Guasti simulati:** il `post_command` fallisce o attende se il caso crea un file sul target. Worker e target vengono fermati o terminati con `docker compose`.
- **Profilo region:** per UC5 i worker generici sono sostituiti da un worker per region (`dbos.region`, `region_routing`), ciascuno solo sulla rete della sua region. L'orchestratore non raggiunge nessun target.
- **Container e Kubernetes:** per UC6 e UC7 il laboratorio avvia un registry delle immagini, due VM con Docker (`vm-a`, `vm-b`) e un cluster k3s a un nodo. L'applicazione di prova è una piccola immagine con `/healthz`; la versione difettosa non ce l'ha, quindi `HEALTHCHECK` e `readinessProbe` restano rossi. Questi servizi partono solo per il loro caso e vengono rimossi alla fine.

## I casi

| Caso | Scenario | Cosa verifica |
|---|---|---|
| **UC1** | Deploy pianificato (`schedule.at`) su un target in una AZ, poi su tre AZ di una region | Attesa fino all'orario, annullamento, idempotenza, release difettosa con rollback automatico, fallimento senza rollback e ripristino, rollback manuale, checksum errato, dry-run, target spento |
| **UC2** | Configurazione di ambiente e promozione | Autenticazione e permessi, conflitti con la configurazione di ambiente, promozione `test → prod` con approvazione, divieto di auto-approvazione, rifiuto, freeze e deroga |
| **UC3** | Più worker sullo stesso database | Un deploy alla volta per ambiente e ambienti in parallelo, worker terminato durante un deploy (recovery automatica dall'altro), arresto ordinato che attende il deploy in corso |
| **UC4** | Rollout a ondate in una region | Canary e approvazione, un'AZ alla volta con bake e health gate, gate rosso sul canary, guasto nell'ultima AZ con `rollback-all`, rifiuto, `onFailure: stop` con drift nell'inventario |
| **UC5** | Rollout a ondate multi-region | Passi e sonde dell'health gate eseguiti dai worker di region, region in parallelo, `rollback-wave`, worker di region spento (il passo lo attende), freeze di una sola region |
| **UC6** | Container su VM con Docker (plugin [container](plugins/container.md)) | Immagine fissata al digest, promozione `test → prod` a ondate, dry-run, immagine unhealthy con `rollback-all`, rollback manuale, VM spenta, tag spostato nel registry (la promozione rifiuta l'immagine diversa) |
| **UC7** | Manifest su Kubernetes (plugin [kubernetes](plugins/kubernetes.md)) | Dry-run lato server, stessi manifest in `test` e `prod` con configurazione di ambiente diversa, rollout bloccato e rollback, prune degli oggetti rimossi, storico delle release, immagine non fissata al digest, rollback manuale |
| **UC8** | [Interfaccia web](guida/interfaccia-web.md) | Intestazioni e configurazione, `whoami`, approvazioni, cursore e stream SSE dell'API; con Playwright: login OIDC, ogni vista verificata con axe in entrambi i temi, dettaglio con log in tempo reale, promozione dopo un dry-run, divieto di auto-approvazione, approvazione da un altro utente seguita in tempo reale, tastiera e italiano |
| **UC9** | [Catene di deploy](guida/catene.md) | Deploy che attende un altro (`after`) senza toccare il proprio target, avvio dopo la fine della dipendenza, dipendenza fallita (`DEPENDENCY_FAILED`) e rifiuto all'invio, release composta in ordine topologico con reinvio idempotente, release fallita con `onFailure: rollback` (deploy riusciti riportati alla versione precedente) |

Ogni verifica stampa ✔ o ✘. Alla fine lo script riassume i fallimenti e salva i log dei container in `test/usecases/.work/compose.log`.

## Aggiungere un caso

Un caso è uno script in `test/usecases/cases/` (`ucN.sh`), eseguito in ordine alfabetico sullo stesso laboratorio. Le funzioni di `lib.sh` creano artefatti e descrittori, eseguono il client con un ruolo (`as cab manent approve <id>`), attendono gli stati (`wait_state`, `wait_rollout`), leggono i target (`current`, `healthy`) e iniettano guasti (`fault`). Ogni caso usa una propria applicazione, così non dipende dagli altri.
