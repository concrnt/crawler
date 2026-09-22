package meili

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// buckets reach the waitTask timeout (10 minutes) so a settings re-index or a
// stalled task still lands in a bucket instead of +Inf only
var meiliWriteDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "crawler",
	Subsystem: "meili",
	Name:      "write_duration_seconds",
	Help:      "Wall time of one Meilisearch write including the wait for its task, by operation (servers, users, communities, posts, activity, delete).",
	Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
}, []string{"op"})

// statsCollector exports the document count of each known index, read from
// Meilisearch at scrape time so an unreachable Meilisearch yields no series
// instead of a stale count.
type statsCollector struct {
	store     *Store
	documents *prometheus.Desc
	indexing  *prometheus.Desc
}

func NewStatsCollector(store *Store) prometheus.Collector {
	return &statsCollector{
		store:     store,
		documents: prometheus.NewDesc("crawler_index_documents", "Documents held by each Meilisearch index.", []string{"index"}, nil),
		indexing:  prometheus.NewDesc("crawler_index_indexing", "1 while Meilisearch is processing tasks for the index.", []string{"index"}, nil),
	}
}

func (s *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.documents
	ch <- s.indexing
}

func (s *statsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stats, err := s.store.Stats(ctx)
	if err != nil {
		slog.Warn("failed to read meilisearch stats for metrics", slog.String("error", err.Error()))
		return
	}
	for _, uid := range []string{ServersIndex, UsersIndex, CommunitiesIndex, PostsIndex} {
		index, ok := stats.Indexes[uid]
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(s.documents, prometheus.GaugeValue, float64(index.NumberOfDocuments), uid)
		indexing := 0.0
		if index.IsIndexing {
			indexing = 1
		}
		ch <- prometheus.MustNewConstMetric(s.indexing, prometheus.GaugeValue, indexing, uid)
	}
}
