# concrnt-crawler

Concrnt public records crawler + Meilisearch search API.

Seed Concrnt server から known servers を取得し、各 server の `net.concrnt.core.replication` (CIP-16) を匿名で追従して users / communities / posts を収集し、Meilisearch に投入します。

- 索引されるのは匿名で読める record だけです (非公開 record は replication 応答から除外されます)。
- `kind: delete` も反映します。`key/*` (配下) と `key*` (自身+配下) の範囲削除にも対応します。
- replication endpoint を持たない server はスキップされます。

既知の制約:

- 公開判定は索引時点です。索引後に非公開化された record は残ります。
- proof の検証は行いません (server が返した record をそのまま信頼します)。
- 同じ key を別 schema で上書きしても、他の索引に入った document は消えません。

## Run

Local compose:

```sh
docker compose up --build
```

Local Go:

```sh
CONCRNT_CRAWLER_CONFIG=config.local.yaml go run .
```

`config.local.yaml` は git ignore 済みです。

## Config

Config path:

1. `CONCRNT_CRAWLER_CONFIG`
2. `config.local.yaml`
3. `config.yaml`
4. `/etc/concrnt-crawler/config.yaml`

Minimal example:

```yaml
server:
  listen: ":8080"
  publicURL: "http://localhost:8080"

crawl:
  seed: "ariake.concrnt.net"
  layer: "concrnt-mainnet"
  knownServersInterval: "10m"
  incrementalInterval: "15m"
  requestTimeout: "15s"
  globalConcurrency: 2
  pageLimit: 100
  overlap: "10s"
  maxPagesPerRun: 1000
  profileSchemas:
    - "https://schema.concrnt.world/p/main.json"
  communitySchemas:
    - "https://schema.concrnt.world/t/community.json"
  postSchemas:
    - "https://schema.concrnt.world/m/markdown.json"
    - "https://schema.concrnt.world/m/reply.json"
    - "https://schema.concrnt.world/m/reroute.json"
    - "https://schema.concrnt.world/m/plaintext.json"
    - "https://schema.concrnt.world/m/media.json"
    - "https://schema.concrnt.world/m/gfm.json"
    - "https://schema.concrnt.world/m/mfm.json"
    - "https://schema.concrnt.world/m/cfm.json"

backends:
  postgresDsn: "postgres://concrnt_crawler:password@db:5432/concrnt_crawler?sslmode=disable"
  meiliHost: "http://meilisearch:7700"
  meiliAPIKey: ""

observability:
  enableTrace: false
  traceEndpoint: ""
```

- `incrementalInterval`: replication feed をポーリングする間隔。追いついていない server は tick を待たずに読み続けます。
- `overlap`: 前回の cursor からどれだけ戻って読み直すか。replication のソートキーは server の受理時刻で、コミット中に採番されるため数秒で十分です。
- `maxPagesPerRun`: 1 run で読むページ数の上限。cursor はページごとに保存されます。

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

### Search Posts

```http
GET /api/v1/search/posts?q=hello&limit=20&offset=0&author=con...&sourceServer=example.net&schema=https://schema.concrnt.world/m/markdown.json
```

`sort` 未指定時は `createdAt:desc` です。hit には `body` のほか record の `value` 全体が入ります。

### Search Servers

```http
GET /api/v1/search/servers?q=ariake&limit=20&offset=0&status=active
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

Returns DB crawler counts and Meilisearch stats. `cursors.catchingUpCount` は replication feed をまだ読み切っていない server の数です。

### Crawl CCFS

```http
POST /api/v1/crawl/ccfs
Content-Type: application/json
```

```json
{
  "ccfs": "ccfs://..."
}
```

You can also post the CCFS URI as `text/plain`.
