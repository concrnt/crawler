package crawler

import (
	"context"
	"log/slog"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gorm.io/gorm"
)

const (
	resultOK        = "ok"
	resultTransient = "transient"
	resultError     = "error"

	subjectCommunity = "community"
	subjectUser      = "user"
)

var (
	replicationRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "replication",
		Name:      "requests_total",
		Help:      "Replication feed requests by server and outcome (ok, transient: 429/503 and the run paused without counting a failure, error).",
	}, []string{"server", "result"})

	replicationPages = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "replication",
		Name:      "pages_total",
		Help:      "Replication pages applied, by server.",
	}, []string{"server"})

	replicationCommits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "replication",
		Name:      "commits_total",
		Help:      "Commits processed from replication pages by server and kind (user, community, post, entry, delete, ack; ignored: not an indexed schema or kind). A record keyed directly under a domain-owned parent counts as entry as well as its own kind; ack covers the four ack kinds of an indexed ack schema.",
	}, []string{"server", "kind"})

	replicationMalformed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "replication",
		Name:      "malformed_total",
		Help:      "Commits skipped because they could not be decoded, by server and kind (commit, profile, community, post, delete, ack).",
	}, []string{"server", "kind"})

	crawlRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "crawl",
		Name:      "runs_total",
		Help:      "Crawl runs over every server, by result (ok, error: at least one server failed).",
	}, []string{"result"})

	crawlRunDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "crawler",
		Subsystem: "crawl",
		Name:      "run_duration_seconds",
		Help:      "Wall time of one crawl run over every server.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	})

	serverCrawls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "server",
		Name:      "crawls_total",
		Help:      "Completed per-server crawls by server and result (ok, error); servers skipped by backoff or layer are not counted.",
	}, []string{"server", "result"})

	// no server label: a per-server histogram is ten series per server for a
	// secondary signal, and per-server throughput is already visible from
	// pages_total and the cursor gauge
	serverCrawlDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "crawler",
		Subsystem: "server",
		Name:      "crawl_duration_seconds",
		Help:      "Wall time of one server's crawl (all replication slices of a run).",
		Buckets:   []float64{0.5, 1, 5, 15, 30, 60, 120, 300, 600, 1800},
	})

	discoveryRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "discovery",
		Name:      "runs_total",
		Help:      "Known-servers discovery runs by result (ok, error).",
	}, []string{"result"})

	activityRefreshes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "crawler",
		Subsystem: "activity",
		Name:      "refreshes_total",
		Help:      "Activity refreshes by subject (community, user) and result (ok, error).",
	}, []string{"subject", "result"})

	activityRefreshDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "crawler",
		Subsystem: "activity",
		Name:      "refresh_duration_seconds",
		Help:      "Wall time of one activity refresh by subject (community, user), including the Meilisearch merge.",
		Buckets:   []float64{0.1, 0.5, 1, 5, 15, 30, 60, 120, 300, 600},
	}, []string{"subject"})
)

// progressCollector exports the replication position of every server in the
// crawled layer. It reads Postgres at scrape time rather than keeping gauges
// in memory: the tables hold a few dozen rows, a server dropped from
// server_states disappears from the output on its own, and a database that
// cannot be read yields no series instead of a stale value.
type progressCollector struct {
	db    *gorm.DB
	layer string

	cursorAt     *prometheus.Desc
	latestPostAt *prometheus.Desc
	caughtUpAt   *prometheus.Desc
	lastFinished *prometheus.Desc
	caughtUp     *prometheus.Desc
	backoff      *prometheus.Desc
	cursorFails  *prometheus.Desc
	serverFails  *prometheus.Desc
	lastCrawled  *prometheus.Desc
	disabled     *prometheus.Desc
}

func NewProgressCollector(db *gorm.DB, layer string) prometheus.Collector {
	serverLabels := []string{"server"}
	return &progressCollector{
		db:           db,
		layer:        layer,
		cursorAt:     prometheus.NewDesc("crawler_replication_cursor_timestamp_seconds", "Replication cursor position (commit receipt time on the source server) as unix seconds; lag is time() minus this.", serverLabels, nil),
		latestPostAt: prometheus.NewDesc("crawler_replication_latest_post_timestamp_seconds", "createdAt of the newest post applied from this server's log as unix seconds, never moved back by a backdated commit; time() minus this is how stale the indexed posts are, where the cursor gauge is how far the log has been read.", serverLabels, nil),
		caughtUpAt:   prometheus.NewDesc("crawler_replication_caught_up_timestamp_seconds", "When the replication feed of this server was last drained, as unix seconds.", serverLabels, nil),
		lastFinished: prometheus.NewDesc("crawler_replication_last_finished_timestamp_seconds", "When the last replication page of this server was applied, as unix seconds.", serverLabels, nil),
		caughtUp:     prometheus.NewDesc("crawler_replication_caught_up", "1 when the last replication run drained the feed, 0 while the crawler is still catching up.", serverLabels, nil),
		backoff:      prometheus.NewDesc("crawler_replication_backoff", "1 while the server or its cursor is skipped by failure backoff.", serverLabels, nil),
		cursorFails:  prometheus.NewDesc("crawler_replication_consecutive_failures", "Consecutive replication failures recorded on the cursor.", serverLabels, nil),
		serverFails:  prometheus.NewDesc("crawler_server_consecutive_failures", "Consecutive crawl failures recorded on the server.", serverLabels, nil),
		lastCrawled:  prometheus.NewDesc("crawler_server_last_crawled_timestamp_seconds", "When the last crawl of this server finished, as unix seconds.", serverLabels, nil),
		disabled:     prometheus.NewDesc("crawler_server_disabled", "1 when the server is excluded from crawling.", serverLabels, nil),
	}
}

func (p *progressCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.cursorAt
	ch <- p.latestPostAt
	ch <- p.caughtUpAt
	ch <- p.lastFinished
	ch <- p.caughtUp
	ch <- p.backoff
	ch <- p.cursorFails
	ch <- p.serverFails
	ch <- p.lastCrawled
	ch <- p.disabled
}

func (p *progressCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// same layer predicate as CrawlOnce: servers of other layers are never
	// crawled and would otherwise show as forever lagging. Disabled servers
	// stay in, flagged by crawler_server_disabled.
	var servers []model.ServerState
	query := p.db.WithContext(ctx)
	if p.layer != "" {
		query = query.Where("layer = ?", p.layer)
	}
	if err := query.Order("domain asc").Find(&servers).Error; err != nil {
		slog.Warn("failed to read server states for metrics", slog.String("error", err.Error()))
		return
	}
	var cursors []model.ReplicationCursor
	if err := p.db.WithContext(ctx).Find(&cursors).Error; err != nil {
		slog.Warn("failed to read replication cursors for metrics", slog.String("error", err.Error()))
		return
	}
	byDomain := make(map[string]model.ReplicationCursor, len(cursors))
	for _, cursor := range cursors {
		byDomain[cursor.ServerDomain] = cursor
	}

	now := time.Now().UTC()
	for _, state := range servers {
		ch <- prometheus.MustNewConstMetric(p.serverFails, prometheus.GaugeValue, float64(state.FailCount), state.Domain)
		ch <- prometheus.MustNewConstMetric(p.disabled, prometheus.GaugeValue, boolValue(state.Disabled), state.Domain)
		if state.LastCrawledAt != nil {
			ch <- prometheus.MustNewConstMetric(p.lastCrawled, prometheus.GaugeValue, float64(state.LastCrawledAt.Unix()), state.Domain)
		}

		cursor, ok := byDomain[state.Domain]
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(p.cursorFails, prometheus.GaugeValue, float64(cursor.FailCount), state.Domain)
		ch <- prometheus.MustNewConstMetric(p.backoff, prometheus.GaugeValue, boolValue(ShouldBackoff(cursor.FailCount, cursor.LastErrorAt, now) || ShouldBackoff(state.FailCount, state.LastErrorAt, now)), state.Domain)
		// a run capped at maxPagesPerRun bumps last_finished_at but not
		// caught_up_at, so the feed is drained only while the two agree
		caughtUp := cursor.CaughtUpAt != nil && (cursor.LastFinishedAt == nil || !cursor.CaughtUpAt.Before(*cursor.LastFinishedAt))
		ch <- prometheus.MustNewConstMetric(p.caughtUp, prometheus.GaugeValue, boolValue(caughtUp), state.Domain)
		if cursor.CursorAt != nil {
			ch <- prometheus.MustNewConstMetric(p.cursorAt, prometheus.GaugeValue, float64(cursor.CursorAt.Unix()), state.Domain)
		}
		if cursor.LatestPostAt != nil {
			ch <- prometheus.MustNewConstMetric(p.latestPostAt, prometheus.GaugeValue, float64(cursor.LatestPostAt.Unix()), state.Domain)
		}
		if cursor.CaughtUpAt != nil {
			ch <- prometheus.MustNewConstMetric(p.caughtUpAt, prometheus.GaugeValue, float64(cursor.CaughtUpAt.Unix()), state.Domain)
		}
		if cursor.LastFinishedAt != nil {
			ch <- prometheus.MustNewConstMetric(p.lastFinished, prometheus.GaugeValue, float64(cursor.LastFinishedAt.Unix()), state.Domain)
		}
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
