package crawler

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// promtool check metrics, as a test: a vec with no children is not gathered,
// so each one is touched with a throwaway label first
func TestMetricsPassLint(t *testing.T) {
	const lintServer = "lint.test"
	replicationRequests.WithLabelValues(lintServer, resultOK)
	replicationPages.WithLabelValues(lintServer)
	replicationCommits.WithLabelValues(lintServer, "post")
	replicationMalformed.WithLabelValues(lintServer, "commit")
	crawlRuns.WithLabelValues(resultOK)
	serverCrawls.WithLabelValues(lintServer, resultOK)
	discoveryRuns.WithLabelValues(resultOK)
	activityRefreshes.WithLabelValues(resultOK)

	problems, err := testutil.GatherAndLint(prometheus.DefaultGatherer,
		"crawler_replication_requests_total",
		"crawler_replication_pages_total",
		"crawler_replication_commits_total",
		"crawler_replication_malformed_total",
		"crawler_crawl_runs_total",
		"crawler_crawl_run_duration_seconds",
		"crawler_server_crawls_total",
		"crawler_server_crawl_duration_seconds",
		"crawler_discovery_runs_total",
		"crawler_activity_refreshes_total",
		"crawler_activity_refresh_duration_seconds",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Errorf("%s: %s", problem.Metric, problem.Text)
	}
}
