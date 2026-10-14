package crawler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt/client"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func loadServerState(t *testing.T, c *Crawler) model.ServerState {
	t.Helper()
	var state model.ServerState
	if err := c.db.First(&state, "domain = ?", replicationDomain).Error; err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCrawlServerSkipsServerDisallowedByRobots(t *testing.T) {
	server := &replicationServer{t: t, robots: "User-agent: *\nDisallow: /private\n\nUser-agent: Concrnt-Crawler\nDisallow: /api/v2/replication\n"}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)
	resetCounters()

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatalf("a disallowed server is skipped, not failed: %v", err)
	}
	if len(server.requests) != 0 {
		t.Fatalf("replication must not be requested: %v", server.requests)
	}
	state := loadServerState(t, c)
	if !state.RobotsDisallowed || state.RobotsCheckedAt == nil || state.FailCount != 0 {
		t.Fatalf("the verdict is recorded on the server state: %+v", state)
	}
	if got := testutil.ToFloat64(robotsRequests.WithLabelValues(replicationDomain, resultOK)); got != 1 {
		t.Errorf("robots_requests_total{result=ok} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(serverCrawls.WithLabelValues(replicationDomain, resultOK)); got != 0 {
		t.Errorf("a skipped server is not a completed crawl: crawls_total{result=ok} = %v, want 0", got)
	}
}

func TestCrawlServerCrawlsWhenRobotsAllows(t *testing.T) {
	cases := map[string]*replicationServer{
		"no robots.txt":      {},
		"other paths denied": {robots: "User-agent: *\nDisallow: /private\n"},
		"other agent denied": {robots: "User-agent: other-bot\nDisallow: /\n"},
		"explicitly allowed": {robots: "User-agent: *\nDisallow: /\n\nUser-agent: concrnt-crawler\nAllow: /api/v2/replication\n"},
	}
	for name, server := range cases {
		t.Run(name, func(t *testing.T) {
			server.t = t
			server.pages = map[string]concrnt.QueryResult{"": {Items: nil, Prev: nil, Next: nil}}
			c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)
			if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
				t.Fatal(err)
			}
			if len(server.requests) != 1 {
				t.Fatalf("replication must be requested once: %v", server.requests)
			}
			state := loadServerState(t, c)
			if state.RobotsDisallowed || state.RobotsCheckedAt == nil {
				t.Fatalf("the allow verdict is recorded too: %+v", state)
			}
		})
	}
}

func TestCrawlServerLiftsRobotsBlockWhenFileChanges(t *testing.T) {
	server := &replicationServer{t: t, robots: "User-agent: *\nDisallow: /\n", pages: map[string]concrnt.QueryResult{"": {}}}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)
	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatal(err)
	}
	if !loadServerState(t, c).RobotsDisallowed {
		t.Fatal("expected the server to be blocked first")
	}

	server.robots = ""
	if err := c.crawlServer(context.Background(), loadServerState(t, c)); err != nil {
		t.Fatal(err)
	}
	if len(server.requests) != 1 || loadServerState(t, c).RobotsDisallowed {
		t.Fatalf("removing robots.txt resumes the crawl on the next run: requests=%v state=%+v", server.requests, loadServerState(t, c))
	}
}

func TestCrawlServerSkipsRunWhenRobotsUnavailable(t *testing.T) {
	server := &replicationServer{t: t, robotsStatus: http.StatusInternalServerError}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)
	resetCounters()

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatalf("an unavailable robots.txt skips the run without a failure: %v", err)
	}
	if len(server.requests) != 0 {
		t.Fatalf("replication must not be requested while robots.txt is undefined: %v", server.requests)
	}
	state := loadServerState(t, c)
	if state.FailCount != 0 || state.RobotsCheckedAt != nil || state.RobotsDisallowed {
		t.Fatalf("no verdict and no failure is recorded: %+v", state)
	}
	if got := testutil.ToFloat64(robotsRequests.WithLabelValues(replicationDomain, resultError)); got != 1 {
		t.Errorf("robots_requests_total{result=error} = %v, want 1", got)
	}
}

func TestCrawlCCFSRefusesSourceDisallowedByRobots(t *testing.T) {
	const domain = "manual.test"
	const ccfs = "ccfs://manual.test/bafyexample"
	store := &manualStore{}
	cl := client.New(domain)
	resolved := false
	cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/concrnt":
			return jsonResponse(t, concrnt.WellKnownConcrnt{
				Domain:    domain,
				Endpoints: map[string]string{resolveEndpoint: "/resolve?uri={uri}"},
			})
		case "/robots.txt":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("User-agent: *\nDisallow: /resolve\n")), Header: make(http.Header)}, nil
		case "/resolve":
			resolved = true
			return jsonResponse(t, testSignedDocument(t, t0))
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
		}
	})

	c := New(newTestDB(t), store, cl, config.Default().Crawl, nil)
	_, err := c.CrawlCCFS(context.Background(), ccfs)
	if err == nil || !strings.Contains(err.Error(), "robots.txt") {
		t.Fatalf("expected a robots.txt refusal, got %v", err)
	}
	if resolved || len(store.calls) != 0 {
		t.Fatalf("the document must not be fetched or indexed: resolved=%v calls=%v", resolved, store.calls)
	}
}
