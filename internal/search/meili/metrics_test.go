package meili

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestStatsCollectorExportsKnownIndexes(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"databaseSize":1,"indexes":{
			"concrnt_posts":{"numberOfDocuments":42,"isIndexing":true,"fieldDistribution":{}},
			"concrnt_users":{"numberOfDocuments":3,"isIndexing":false,"fieldDistribution":{}},
			"unrelated":{"numberOfDocuments":9,"isIndexing":false,"fieldDistribution":{}}}}`))
	}))
	defer server.Close()
	collector := NewStatsCollector(New(NewClient(server.URL, ""), 0, nil))

	if problems, err := testutil.CollectAndLint(collector); err != nil || len(problems) > 0 {
		t.Fatalf("lint: %v %v", problems, err)
	}
	expected := `
# HELP crawler_index_documents Documents held by each Meilisearch index.
# TYPE crawler_index_documents gauge
crawler_index_documents{index="concrnt_posts"} 42
crawler_index_documents{index="concrnt_users"} 3
# HELP crawler_index_indexing 1 while Meilisearch is processing tasks for the index.
# TYPE crawler_index_indexing gauge
crawler_index_indexing{index="concrnt_posts"} 1
crawler_index_indexing{index="concrnt_users"} 0
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}

	// an unreachable Meilisearch yields no series rather than stale counts
	status = http.StatusInternalServerError
	if got := testutil.CollectAndCount(collector); got != 0 {
		t.Fatalf("expected no metrics on error, got %d", got)
	}
}
