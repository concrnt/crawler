package crawler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProgressCollectorExportsPerServerReplicationState(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	const layer = "concrnt-mainnet"
	recentError := time.Now().UTC().Add(-time.Minute)

	servers := []model.ServerState{
		{Domain: "caught.test", Layer: layer, LastCrawledAt: &t2},
		{Domain: "capped.test", Layer: layer, LastCrawledAt: &t1},
		{Domain: "fresh.test", Layer: layer},
		{Domain: "failing.test", Layer: layer, FailCount: 1, LastCrawledAt: &t0},
		{Domain: "disabled.test", Layer: layer, Disabled: true, LastCrawledAt: &t0},
		{Domain: "other.test", Layer: "concrnt-testnet", LastCrawledAt: &t0},
	}
	cursors := []model.ReplicationCursor{
		// drained: caught_up_at and last_finished_at were stamped together
		{ServerDomain: "caught.test", CursorAt: &t0, LatestPostAt: &t0, CaughtUpAt: &t1, LastFinishedAt: &t1},
		// hit maxPagesPerRun since the last drain
		{ServerDomain: "capped.test", CursorAt: &t1, LatestPostAt: &t1, CaughtUpAt: &t0, LastFinishedAt: &t1},
		// two failures a minute ago: inside the backoff ladder
		{ServerDomain: "failing.test", FailCount: 2, LastErrorAt: &recentError},
		{ServerDomain: "disabled.test", CursorAt: &t0, CaughtUpAt: &t0, LastFinishedAt: &t0},
		{ServerDomain: "other.test", CursorAt: &t0, CaughtUpAt: &t0, LastFinishedAt: &t0},
	}
	if err := c.db.Create(&servers).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.db.Create(&cursors).Error; err != nil {
		t.Fatal(err)
	}

	collector := NewProgressCollector(c.db, layer)
	if problems, err := testutil.CollectAndLint(collector); err != nil || len(problems) > 0 {
		t.Fatalf("lint: %v %v", problems, err)
	}

	expected := fmt.Sprintf(`
# HELP crawler_replication_backoff 1 while the server or its cursor is skipped by failure backoff.
# TYPE crawler_replication_backoff gauge
crawler_replication_backoff{server="capped.test"} 0
crawler_replication_backoff{server="caught.test"} 0
crawler_replication_backoff{server="disabled.test"} 0
crawler_replication_backoff{server="failing.test"} 1
# HELP crawler_replication_caught_up 1 when the last replication run drained the feed, 0 while the crawler is still catching up.
# TYPE crawler_replication_caught_up gauge
crawler_replication_caught_up{server="capped.test"} 0
crawler_replication_caught_up{server="caught.test"} 1
crawler_replication_caught_up{server="disabled.test"} 1
crawler_replication_caught_up{server="failing.test"} 0
# HELP crawler_replication_caught_up_timestamp_seconds When the replication feed of this server was last drained, as unix seconds.
# TYPE crawler_replication_caught_up_timestamp_seconds gauge
crawler_replication_caught_up_timestamp_seconds{server="capped.test"} %[1]d
crawler_replication_caught_up_timestamp_seconds{server="caught.test"} %[2]d
crawler_replication_caught_up_timestamp_seconds{server="disabled.test"} %[1]d
# HELP crawler_replication_cursor_timestamp_seconds Replication cursor position (commit receipt time on the source server) as unix seconds; lag is time() minus this.
# TYPE crawler_replication_cursor_timestamp_seconds gauge
crawler_replication_cursor_timestamp_seconds{server="capped.test"} %[2]d
crawler_replication_cursor_timestamp_seconds{server="caught.test"} %[1]d
crawler_replication_cursor_timestamp_seconds{server="disabled.test"} %[1]d
# HELP crawler_replication_consecutive_failures Consecutive replication failures recorded on the cursor.
# TYPE crawler_replication_consecutive_failures gauge
crawler_replication_consecutive_failures{server="capped.test"} 0
crawler_replication_consecutive_failures{server="caught.test"} 0
crawler_replication_consecutive_failures{server="disabled.test"} 0
crawler_replication_consecutive_failures{server="failing.test"} 2
# HELP crawler_replication_last_finished_timestamp_seconds When the last replication page of this server was applied, as unix seconds.
# TYPE crawler_replication_last_finished_timestamp_seconds gauge
crawler_replication_last_finished_timestamp_seconds{server="capped.test"} %[2]d
crawler_replication_last_finished_timestamp_seconds{server="caught.test"} %[2]d
crawler_replication_last_finished_timestamp_seconds{server="disabled.test"} %[1]d
# HELP crawler_replication_latest_post_timestamp_seconds createdAt of the newest post applied from this server's log as unix seconds, never moved back by a backdated commit; time() minus this is how stale the indexed posts are, where the cursor gauge is how far the log has been read.
# TYPE crawler_replication_latest_post_timestamp_seconds gauge
crawler_replication_latest_post_timestamp_seconds{server="capped.test"} %[2]d
crawler_replication_latest_post_timestamp_seconds{server="caught.test"} %[1]d
# HELP crawler_server_disabled 1 when the server is excluded from crawling.
# TYPE crawler_server_disabled gauge
crawler_server_disabled{server="capped.test"} 0
crawler_server_disabled{server="caught.test"} 0
crawler_server_disabled{server="disabled.test"} 1
crawler_server_disabled{server="failing.test"} 0
crawler_server_disabled{server="fresh.test"} 0
# HELP crawler_server_consecutive_failures Consecutive crawl failures recorded on the server.
# TYPE crawler_server_consecutive_failures gauge
crawler_server_consecutive_failures{server="capped.test"} 0
crawler_server_consecutive_failures{server="caught.test"} 0
crawler_server_consecutive_failures{server="disabled.test"} 0
crawler_server_consecutive_failures{server="failing.test"} 1
crawler_server_consecutive_failures{server="fresh.test"} 0
# HELP crawler_server_last_crawled_timestamp_seconds When the last crawl of this server finished, as unix seconds.
# TYPE crawler_server_last_crawled_timestamp_seconds gauge
crawler_server_last_crawled_timestamp_seconds{server="capped.test"} %[2]d
crawler_server_last_crawled_timestamp_seconds{server="caught.test"} %[3]d
crawler_server_last_crawled_timestamp_seconds{server="disabled.test"} %[1]d
crawler_server_last_crawled_timestamp_seconds{server="failing.test"} %[1]d
`, t0.Unix(), t1.Unix(), t2.Unix())
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
