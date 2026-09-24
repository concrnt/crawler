# concrnt-crawler

Concrnt public records crawler + Meilisearch search API.

Seed Concrnt server から known servers を取得し、各 server の `net.concrnt.core.replication` (CIP-16) を匿名で追従して users / communities / posts を収集し、Meilisearch に投入します。

- 索引されるのは匿名で読める record だけです (非公開 record は replication 応答から除外されます)。
- community はドメイン所有 (`cckv://<FQDN>/...`) のものだけを索引します。owner が CCID のユーザー所有 community は当面検索対象外で、replication では警告ログを出してスキップ、manual crawl ではエラーになります。
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
  internalListen: ":8081"

crawl:
  seed: "ariake.concrnt.net"
  layer: "concrnt-mainnet"
  knownServersInterval: "10m"
  incrementalInterval: "15m"
  requestTimeout: "30s"
  globalConcurrency: 2
  pageLimit: 100
  overlap: "10s"
  maxPagesPerRun: 1000
  profileSchemas:
    - "https://schema.concrnt.world/p/main.json"
  communitySchemas:
    - "https://schema.concrnt.world/t/community.json"
  ackSchemas:
    - "https://schema.concrnt.world/ack/follow.json"
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

- `internalListen`: 運用用リスナー (`/metrics`, `/health`)。Prometheus が scrape する内部向けで、外部には公開しないでください (既定 `:8081`)。
- `incrementalInterval`: replication feed をポーリングする間隔。追いついていない server は tick を待たずに読み続けます。
- `overlap`: 前回の cursor からどれだけ戻って読み直すか。replication のソートキーは server の受理時刻で、コミット中に採番されるため数秒で十分です。
- `maxPagesPerRun`: 1 run で読むページ数の上限。cursor はページごとに保存されます。
- `activityInterval`: コミュニティ / ユーザーの活動量を再集計して索引へ書き込む間隔 (既定 10m)。
- `activityHalfLife`: `activityScore` の半減期 (既定 168h = 7 日)。
- `activityHistoryDays`: `activityHistory` (日別の活動量) に持つ日数 (既定 30)。
- `ackSchemas`: フォロー関係として取り込む ack の schema (既定 `ack/follow.json` のみ)。

## Community activity

コミュニティ検索を「アクティブ順」に並べるため、投稿の着信量をクロール時に事前集計しています。

- コミュニティ key の直下に着信した record (通常は CIP-7 配送が残す reference record `<community>/<cdid>`) を、
  schema を問わず 1 件の活動として Postgres (`community_entries`) に積みます。reply / reroute も含みます。
  親 key が索引済みコミュニティでない record は捨てます。着信時刻は record の `createdAt` (reference なら配送元サーバーが
  配送を作った時刻) で、同じ key の再 commit (編集) では据え置きます。編集は活動に数えません。
- `activityInterval` ごとに索引済みコミュニティ全件について次を計算し、`concrnt_communities` の文書へ部分更新で書き込みます。
  - `activityScore`: 直近 30 日の着信について `2^(-経過時間/activityHalfLife)` を合計した値
  - `postCount7d` / `postCount30d`: 直近 7 日 / 30 日の着信数
  - `activeAuthors7d`: 直近 7 日のユニーク投稿者数
  - `lastPostAt`: 最新の着信時刻 (30 日より前でも出ます。着信が無ければ省略)
  - `activityHistory`: 日別 (UTC) の `{date: "YYYY-MM-DD", posts, authors}` を古い日から今日まで `activityHistoryDays` 個。
    着信の無い日も 0 で入り、末尾は当日 (集計時点までの途中経過) です。アクティビティグラフの描画用。
- `kind: delete` (単一 key / `key/*` / `key*`) は entries にも反映されます。元投稿の削除に伴う reference の掃除はサーバー内部で行われ
  replication には reference key の delete として現れないため (CIP-4 §6.1)、reference の `value.href` も削除対象の照合に使います。
- 既に稼働している環境へ入れる場合、過去分は `replication_cursors` を削除して再クロールしてください (互換処理はありません)。

## User activity

ユーザー検索を「アクティブ順」に並べ、閲覧者のフォロー先に絞った順位 (下記 Follows) を出すため、投稿量もユーザー単位で事前集計しています。

- `postSchemas` に一致した投稿 record 本体を、`author` (CCID) の活動 1 件として Postgres (`user_entries`) に積みます。
  配送先に残る reference は数えないので、いくつのタイムラインに配送されても 1 投稿 = 1 活動です。
  非公開タイムラインへの投稿は replication に出てこないため、集計は公開の投稿活動だけになります。
  同じ key の再 commit では据え置きます。
- ユーザー索引の鏡 (`indexed_users`: プロフィール key → CCID) を持ち、`activityInterval` ごとに索引済み全 CCID について
  `activityScore` / `postCount7d` / `postCount30d` / `lastPostAt` / `activityHistory` (`{date, posts}`) を計算し、
  その CCID のプロフィール文書 (main とサブプロフィール) すべてへ部分更新で書き込みます。計算はコミュニティと同じです。
  索引済みユーザーが数万規模になったら差分更新にする余地があります。
- `kind: delete` は `user_entries` (投稿 key) と `indexed_users` (プロフィール key) にも反映されます。

## Follows

閲覧者のフォロー先に絞った順位付け (`viewer` パラメータ) のため、フォロー関係もクロールしています。

- replication に流れる `ack` / `unack` (承認側サーバー) と `acked` / `unacked` (被承認側サーバー、CIP-10 §5.2) を、
  `(acker, ackee, schema)` の 3 つ組ごとに 1 行として Postgres (`acks`) に反映します。これらの commit は認可評価なしで匿名にも返ります (CIP-16 §3.4)。
- 状態遷移はサーバーと同じ規則です (CIP-10 §4): 保存済みより `createdAt` が厳密に新しい Document だけが適用され、
  unack は行を無効化します (削除しません)。両側のサーバーから同じ関係が 2 回来ても、再生で古い Document が来ても収束します。
- `ackSchemas` にない schema の ack は無視します。`associate` が `cckv://<CCID>` でないもの、`key` を持つもの、
  `author` が CCID でないものは malformed として捨てます。
- ack は key を持たないため delete の対象になりません。退会 (ハード削除) は replication にイベントが出ないので、
  ユーザー / 投稿の索引と同様に残ります。
- 既に稼働している環境へ入れる場合、`user_entries` ともども `replication_cursors` を削除して再クロールしてください。

## Metrics

`internalListen` (既定 `:8081`) の `GET /metrics` で Prometheus 形式のメトリクスを返します。クロール対象ごとの進捗は `server` ラベル (FQDN) 付きのゲージで、scrape のたびに Postgres の `server_states` / `replication_cursors` を読んで出します。

| metric | 内容 |
| --- | --- |
| `crawler_replication_cursor_timestamp_seconds{server}` | replication cursor の位置 (server 側の受理時刻)。遅延は `time() - この値` |
| `crawler_replication_latest_post_timestamp_seconds{server}` | その server の log から索引した投稿の最新 `createdAt` (単調増加: backdate された commit で戻らない)。cursor が「log をどこまで読んだか」なのに対し、こちらは「索引済みの投稿がどれだけ新しいか」。遅延は `time() - この値` |
| `crawler_replication_caught_up{server}` | 直近の run で feed を読み切っていれば 1、追いつき中なら 0 |
| `crawler_replication_caught_up_timestamp_seconds{server}` / `crawler_replication_last_finished_timestamp_seconds{server}` | 最後に読み切った時刻 / 最後にページを適用した時刻 |
| `crawler_replication_backoff{server}` | 失敗の backoff でスキップ中なら 1 |
| `crawler_replication_consecutive_failures{server}` / `crawler_server_consecutive_failures{server}` | cursor / server に記録された連続失敗回数 |
| `crawler_server_last_crawled_timestamp_seconds{server}` / `crawler_server_disabled{server}` | 最後にクロールした時刻 / クロール対象外なら 1 |
| `crawler_replication_requests_total{server,result}` | replication 要求数 (`ok` / `transient` = 429・503 / `error`) |
| `crawler_replication_pages_total{server}` / `crawler_replication_commits_total{server,kind}` / `crawler_replication_malformed_total{server,kind}` | 適用したページ数 / 処理した commit 数 (`user` `community` `post` `entry` `delete` `ack` `ignored`) / 解釈できず捨てた commit 数 |
| `crawler_crawl_runs_total{result}` / `crawler_crawl_run_duration_seconds` | 全 server を回す run の回数と所要時間 |
| `crawler_server_crawls_total{server,result}` / `crawler_server_crawl_duration_seconds` | server 単位のクロール回数と所要時間 |
| `crawler_discovery_runs_total{result}` / `crawler_activity_refreshes_total{subject,result}` / `crawler_activity_refresh_duration_seconds{subject}` | known-servers 取得 / 活動量再集計 (`community` / `user`) の回数と所要時間 |
| `crawler_index_documents{index}` / `crawler_index_indexing{index}` | Meilisearch 各索引の文書数 / 索引処理中なら 1 |
| `crawler_meili_write_duration_seconds{op}` | Meilisearch 書き込み (task 完了待ち込み) の所要時間 |
| `crawler_build_info{version}` | 常に 1。ラベルにバージョン |

server 総数や追いつき済みの数は PromQL で `count(crawler_server_disabled == 0)` / `count(crawler_replication_caught_up == 1)` のように導出してください。
concrnt client 由来の `concrnt_peer_requests_total{host,code}` / `concrnt_peer_request_duration_seconds{host}` と Go / `go_sql_*` の標準メトリクスも同じエンドポイントに出ます。

Grafana ダッシュボードは [`docs/dashboards/crawler.json`](docs/dashboards/crawler.json) にあります。

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

`sort` は `createdAt` / `indexedAt` / `username` に加えて `activityScore` / `postCount7d` / `postCount30d` / `lastPostAt` を受け付けます (未指定時は `createdAt:desc`)。hit には User activity の集計フィールドが (集計 tick 未到達の文書を除いて) 入ります。ユーザーとコミュニティの `createdAt` は、そのキーで最初に索引した record の `createdAt` (より古い `createdAt` の record が後から来た場合はそれ) を保持します。プロフィールの編集や、長期間オフラインだったサーバーのログ再生で新着扱いにはなりません。`indexedAt` は最後に索引した時刻です。

### Search Communities

```http
GET /api/v1/search/communities?q=general&limit=20&offset=0&sourceServer=example.net&owner=example.net&sort=activityScore
```

`cckv` を指定すると、そのコミュニティ 1 件を (活動量フィールドごと) 返します。

`sort` は `createdAt` / `indexedAt` / `name` に加えて `activityScore` / `postCount7d` / `postCount30d` / `activeAuthors7d` / `lastPostAt` を受け付けます (方向省略時は `desc`、未指定時は `createdAt:desc`)。`sort=activityScore` がアクティブ順です。

### Viewer mode (フォロー中でアクティブ)

```http
GET /api/v1/search/users?viewer=con...&limit=20&offset=0
GET /api/v1/search/communities?viewer=con...&limit=20&offset=0
GET /api/v1/search/communities?viewer=con...&q=hello&limit=20&offset=0
```

`viewer` (CCID) を指定すると、その閲覧者がフォローしている (`ackSchemas` の有効な ack がある) ユーザーの直近 30 日の投稿から
リクエスト時に順位付けします。スコアは `activityScore` と同じ式 (`2^(-経過時間/activityHalfLife)` の合計) で、同点は key の昇順です。
`sort` は受け付けません (400)。`sourceServer` / `owner` / `cckv` は無視します。
`q` を付けるとキーワード検索の並び順として働きます: 順位付けされた key のうち検索に一致した文書をスコア順に先頭へ並べ、
その後ろに残りの一致文書を関連度順で続けます (フォロー先の活動が無い一致も落としません)。ページは両者をまたいで切り、
`estimatedTotalHits` は「順位付け側の一致件数 + 残りの推定件数」です。users は `q` ありのとき一致したプロフィール文書を
すべて hit にします (サブプロフィールが一致した場合はそれ自体が hit)。

- users: フォロー先のうち索引済みのユーザーを、本人の投稿量で並べます。hit は CCID につき 1 文書 (main プロフィール優先) で、
  グローバル集計のフィールドに加えて `followeeScore` / `followeePostCount30d` が入ります。
- communities: フォロー先の投稿 (reference) が着信した索引済みコミュニティを並べます。hit には `followeeScore` /
  `followeePostCount30d` と、そのコミュニティに多く投稿したフォロー先を投稿数順 (同数は CCID 順) に最大 5 人並べた `topAuthors` が入ります。
- フォロー先が無い、または直近 30 日に活動が無ければ `hits` は空です。`estimatedTotalHits` は順位付けされた件数です。

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
