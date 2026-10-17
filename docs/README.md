# Documentazione di manent

Sorgenti del sito statico della documentazione, generato con [MkDocs](https://www.mkdocs.org/) e il tema [Material](https://squidfunk.github.io/mkdocs-material/).

```
docs/
├── mkdocs.yml        # configurazione del sito e menu di navigazione
├── requirements.txt  # versioni di MkDocs, del tema e del plugin i18n
├── content/          # pagine in Markdown: pagina.md (inglese) e pagina.it.md (italiano)
└── site/             # sito generato (non versionato)
```

## Generare il sito

```sh
python3 -m venv docs/.venv
docs/.venv/bin/pip install -r docs/requirements.txt
PATH="$PWD/docs/.venv/bin:$PATH" make docs        # genera docs/site
PATH="$PWD/docs/.venv/bin:$PATH" make docs-serve  # anteprima su http://<host>:8000/manent/ (tutte le interfacce; DOCS_ADDR=127.0.0.1:8000 solo locale)
```

La build usa `strict: true`: un link interno rotto o una pagina fuori dal menu fanno fallire la generazione.

## Pubblicare

`docs/site` è un sito statico autonomo: basta copiarlo su un qualsiasi web server o servizio di hosting statico (GitHub Pages, GitLab Pages, Netlify, un bucket S3, nginx).

Con GitHub Pages, dalla radice del repository:

```sh
docs/.venv/bin/mkdocs gh-deploy -f docs/mkdocs.yml   # pubblica sul branch gh-pages
```

Se il sito non viene servito da `https://joeg-ita.github.io/manent/`, aggiornare `site_url` in `mkdocs.yml`.

## Tema

Material for MkDocs, scuro di default, con logo e favicon in `content/assets/` e personalizzazioni in `content/stylesheets/extra.css`: colori (variabili `--md-*` e `--mn-*`), font (Inter, Space Grotesk per i titoli, JetBrains Mono per il codice), griglia di sfondo, card e la sezione hero della home (`mn-hero`, `mn-terminal`, `mn-stats` in `content/index.md`).

## Lingue

Il sito è in inglese (lingua di default, alla radice) e in italiano (sotto `/it/`), con il plugin [mkdocs-static-i18n](https://github.com/ultrabug/mkdocs-static-i18n) in modalità `suffix`: ogni pagina `pagina.md` (inglese) ha la traduzione `pagina.it.md` nella stessa directory. I percorsi restano gli stessi nelle due lingue, così il selettore della lingua porta alla stessa pagina.

- Il menu (`nav`) è in inglese; le voci in italiano sono in `nav_translations` (`plugins.i18n` di `mkdocs.yml`): una voce nuova del menu va tradotta lì.
- I link tra pagine sono uguali nelle due lingue (`../guida/callback.md`): il plugin li risolve verso la pagina della stessa lingua.
- Un titolo inglese a cui puntano link con ancora riporta l'id del titolo italiano, così le ancore valgono in entrambe le lingue: `## URL limits { #limiti-sugli-url }`.
- Messaggi di errore e output dei comandi restano quelli reali del programma: gli errori sono in inglese, i log e l'output dei comandi in italiano.
- `navigation.instant` non è attivo perché impedirebbe al selettore della lingua di restare sulla stessa pagina.

## Scrivere la documentazione

- Una pagina nuova va aggiunta anche alla sezione `nav` di `mkdocs.yml` e scritta in entrambe le lingue.
- I link tra pagine sono relativi e puntano al file `.md` (es. `[callback](../guida/callback.md)`).
- I riferimenti al codice si scrivono come percorsi (`internal/engine/worker.go`), non come link: i file fuori da `content/` non fanno parte del sito.
- Per i diagrammi si usano blocchi ` ```mermaid `.
