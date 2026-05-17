# concrnt-search

Concrnt public records crawler + Meilisearch search API.

Seed Concrnt server から known servers を取得し、各 server の `net.concrnt.core.query` で users / communities を収集して Meilisearch に投入します。

## Run

Local compose:

```sh
docker compose up --build
```

Local Go:

```sh
CONCRNT_SEARCH_CONFIG=config.local.yaml go run .
```

`config.local.yaml` は git ignore 済みです。

## Config

Config path:

1. `CONCRNT_SEARCH_CONFIG`
2. `config.local.yaml`
3. `config.yaml`
4. `/etc/concrnt-search/config.yaml`

Minimal example:

```yaml
server:
  listen: ":8080"
  publicURL: "http://localhost:8080"
  adminToken: ""

crawl:
  seed: "ariake.concrnt.net"
  prefix: "cckv://"
  knownServersInterval: "10m"
  incrementalInterval: "15m"
  requestTimeout: "15s"
  globalConcurrency: 2
  perServerConcurrency: 1
  pageLimit: 100
  overlap: "2m"
  maxPagesPerRun: 20
  profileSchemas:
    - "https://schema.concrnt.world/p/main.json"
  communitySchemas:
    - "https://schema.concrnt.world/t/community.json"

backends:
  postgresDsn: "postgres://concrnt_search:password@db:5432/concrnt_search?sslmode=disable"
  meiliHost: "http://meilisearch:7700"
  meiliAPIKey: ""

observability:
  enableTrace: false
  traceEndpoint: ""
```

## API

### Health

```http
GET /health
```

```json
{"status":"ok"}
```

### Search Users

```http
GET /api/v1/search/users?q=alice&limit=20&offset=0&sourceServer=example.net&owner=con...
```

### Search Communities

```http
GET /api/v1/search/communities?q=general&limit=20&offset=0&sourceServer=example.net&owner=con...
```

### Search Servers

```http
GET /api/v1/search/servers?q=ariake&limit=20&offset=0&layer=concrnt&status=active
```

Search response:

```json
{
  "hits": [],
  "query": "alice",
  "limit": 20,
  "offset": 0,
  "estimatedTotalHits": 0,
  "processingTimeMs": 0
}
```

### Stats

```http
GET /api/v1/stats
```

Returns DB crawler counts and Meilisearch stats.

