# stretchy

**stretchy** è un mini motore di ricerca compatibile con il protocollo
Elasticsearch, scritto in Go, pensato per ottimizzare le ricerche di
WordPress / WooCommerce (ElasticPress e plugin simili) su un singolo
server, senza il peso della JVM.

- API REST compatibile con Elasticsearch 8.x (si presenta come `8.17.0`, API typeless, header `X-Elastic-Product`, media type `compatible-with`)
- indice invertito in memoria con scoring BM25, persistenza su disco (WAL + compattazione)
- query DSL: `match`, `match_phrase`, `multi_match`, `term`, `terms`, `range`,
  `exists`, `prefix`, `wildcard`, `fuzzy` (AUTO), `ids`, `bool`,
  `constant_score`, `function_score`, `dis_max`, `nested`, `query_string`
- aggregazioni: `terms`, `filter`, `filters`, `range`, `histogram`,
  `date_histogram`, `global`, `min`/`max`/`avg`/`sum`/`stats`/`value_count`/`cardinality`
- `_bulk`, `_update` (merge parziale + upsert), `_mget`, `_delete_by_query`,
  `_count`, `_analyze`, highlight, sort, paginazione, `post_filter`, `_source` filtering
- analisi del testo con folding degli accenti latini ("caffè" ↔ "caffe")
- binario singolo, configurazione YAML, basic auth opzionale

Non è un cluster: un nodo, uno shard, nessuna replica. Per un catalogo
WooCommerce da decine di migliaia di prodotti è più che sufficiente.

## Installazione (Ubuntu)

```bash
sudo ./stretchy --init
sudo systemctl start stretchy
curl http://127.0.0.1:9200/
```

`--init` esegue:

1. copia del binario in `/sbin/stretchy`
2. creazione dell'utente di sistema `stretchy`
3. creazione di `/etc/stretchy/config.yaml` (se assente), `/var/lib/stretchy`, `/var/log/stretchy`
4. installazione della unit systemd `stretchy.service` (abilitata al boot)
5. regola logrotate in `/etc/logrotate.d/stretchy`

## Configurazione

`/etc/stretchy/config.yaml` (vedi [config.example.yaml](config.example.yaml)):

```yaml
server:
  host: 127.0.0.1
  port: 9200

auth:            # opzionale
  username: elastic
  password: change-me

storage:
  data_dir: /var/lib/stretchy

logging:
  dir: /var/log/stretchy
  level: info
```

## Uso con WordPress / ElasticPress

In `wp-config.php`:

```php
define( 'EP_HOST', 'http://127.0.0.1:9200' );
// con basic auth:
// define( 'EP_HOST', 'http://elastic:change-me@127.0.0.1:9200' );
```

poi indicizza:

```bash
wp elasticpress index --setup
```

## Sviluppo

```bash
make test          # go test -race ./...
make build         # binario locale in bin/
make build-linux   # cross-compile linux/amd64
```

Senza `/etc/stretchy/config.yaml`, stretchy parte con i default di
sviluppo (dati in `./data`, log su stderr).

## CI / Release

- **CI** (`.github/workflows/ci.yml`): a ogni push su `main` esegue vet,
  test e compila i binari `linux/amd64` e `linux/arm64` come artifact.
- **Release** (`.github/workflows/release.yml`): al push di un tag `v*`
  GoReleaser pubblica gli archivi tar.gz con i binari e i checksum.

```bash
git tag v0.1.0 && git push origin v0.1.0
```
