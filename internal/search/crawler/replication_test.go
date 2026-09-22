package crawler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/database"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt/client"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	replicationDomain = "replication.test"
	testAuthor        = "con012345678901234567890123456789012345678"
	postsParent       = "cckv://" + testAuthor + "/concrnt.world/profiles/main/posts"
)

var (
	t0 = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Second)
	t2 = t0.Add(2 * time.Second)
)

// replicationServer stubs a concrnt server: the well-known advertises the
// replication endpoint and each page is keyed by the since parameter the
// crawler is expected to send ("" for the first page of a fresh cursor).
type replicationServer struct {
	t        *testing.T
	pages    map[string]concrnt.QueryResult
	status   int
	requests []string
}

func (s *replicationServer) RoundTrip(r *http.Request) (*http.Response, error) {
	switch r.URL.Path {
	case "/.well-known/concrnt":
		return jsonResponse(s.t, concrnt.WellKnownConcrnt{
			Domain: replicationDomain,
			Endpoints: map[string]string{
				replicationEndpoint: "/api/v2/replication{?owner,since,until,limit,order}",
			},
		})
	case "/api/v2/replication":
		s.requests = append(s.requests, r.URL.RawQuery)
		if s.status != 0 {
			return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		q := r.URL.Query()
		if q.Get("order") != "asc" || q.Get("limit") != "100" || q.Has("until") {
			s.t.Fatalf("unexpected replication query: %s", r.URL.RawQuery)
		}
		page, ok := s.pages[q.Get("since")]
		if !ok {
			s.t.Fatalf("unexpected since: %q (query %s)", q.Get("since"), r.URL.RawQuery)
		}
		return jsonResponse(s.t, page)
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
	}
}

func newReplicationCrawler(t *testing.T, server *replicationServer, store Store, cfg config.Crawl) *Crawler {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// every pooled connection would otherwise open its own empty in-memory db
	sqlDB.SetMaxOpenConns(1)
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	cl := client.New(replicationDomain)
	cl.GetClient().Transport = server
	return New(db, store, cl, cfg, nil)
}

func stamp(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

func commit(t *testing.T, kind string, key string, schema string, value any, ccfs string) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[any]{
		Kind:      kind,
		Key:       key,
		Value:     value,
		Author:    testAuthor,
		Schema:    schema,
		CreatedAt: t0,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return concrnt.SignedDocument{CCFS: &ccfs, Document: string(raw), Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}}
}

func loadCursor(t *testing.T, c *Crawler) model.ReplicationCursor {
	t.Helper()
	var cursor model.ReplicationCursor
	if err := c.db.First(&cursor, "server_domain = ?", replicationDomain).Error; err != nil {
		t.Fatal(err)
	}
	return cursor
}

func TestCrawlServerAppliesCommitsInLogOrder(t *testing.T) {
	post1 := postsParent + "/p1"
	post2 := postsParent + "/p2"
	server := &replicationServer{t: t, pages: map[string]concrnt.QueryResult{
		"": {
			Items: []concrnt.SignedDocument{
				commit(t, "record", post1, config.DefaultPostSchemas[0], map[string]string{"body": "first"}, "ccfs://a/concrnt/1"),
				commit(t, "record", "cckv://"+testAuthor+"/concrnt.world/profiles/main", config.DefaultProfileSchema, map[string]string{"username": "alice"}, "ccfs://a/concrnt/2"),
				commit(t, "record", "cckv://example.com/t/general", config.DefaultCommunitySchema, map[string]string{"name": "general"}, "ccfs://a/concrnt/3"),
				commit(t, "record", "cckv://"+testAuthor+"/t/mine", config.DefaultCommunitySchema, map[string]string{"name": "user-owned"}, "ccfs://a/concrnt/3u"),
				commit(t, "entity", "cckv://"+testAuthor, "https://schema.concrnt.net/entity.json", map[string]string{"domain": replicationDomain}, "ccfs://a/concrnt/4"),
				commit(t, "association", "", "https://schema.concrnt.world/a/like.json", map[string]string{}, "ccfs://a/concrnt/5"),
				{Document: "{not json", Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}},
				commit(t, "record", post1, config.DefaultPostSchemas[0], map[string]string{"body": "second"}, "ccfs://a/concrnt/6"),
				commit(t, "record", postsParent+"/list", "https://schema.concrnt.world/s/list.json", map[string]string{"name": "ignored"}, "ccfs://a/concrnt/7"),
			},
			Prev: &t0,
			Next: &t1,
		},
		stamp(t1): {
			Items: []concrnt.SignedDocument{
				commit(t, "delete", "", "", post1, "ccfs://a/concrnt/8"),
				commit(t, "record", post2, config.DefaultPostSchemas[0], map[string]string{"body": "third"}, "ccfs://a/concrnt/9"),
			},
			Prev: &t1,
			Next: nil,
		},
	}}
	store := &manualStore{}
	c := newReplicationCrawler(t, server, store, config.Default().Crawl)

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{"users", "communities", "posts", "delete:concrnt_users", "delete:concrnt_communities", "delete:concrnt_posts", "posts"}
	if strings.Join(store.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("store call order mismatch:\n got: %v\nwant: %v", store.calls, wantCalls)
	}
	if len(store.posts) != 2 || store.posts[0].Body != "second" || store.posts[1].Body != "third" {
		t.Fatalf("expected the last document per key, got %+v", store.posts)
	}
	if len(store.users) != 1 || store.users[0].Username != "alice" || len(store.communities) != 1 || store.communities[0].Name != "general" {
		t.Fatalf("profile/community not indexed (user-owned community must be skipped): users=%+v communities=%+v", store.users, store.communities)
	}
	if len(store.deletes) != 3 || len(store.deletes[0].IDs) != 1 || store.deletes[0].Ancestor != "" {
		t.Fatalf("unexpected delete specs: %+v", store.deletes)
	}
	if len(server.requests) != 2 || strings.Contains(server.requests[0], "since=") {
		t.Fatalf("unexpected requests: %v", server.requests)
	}

	cursor := loadCursor(t, c)
	if cursor.CursorAt == nil || !cursor.CursorAt.Equal(t1) {
		t.Fatalf("cursor should be the prev of the drained page (%s), got %v", stamp(t1), cursor.CursorAt)
	}
	if cursor.CaughtUpAt == nil || cursor.FailCount != 0 {
		t.Fatalf("cursor should be caught up without failures: %+v", cursor)
	}
}

func TestReplicateResumesBehindCursorAndContinuesPastEmptyPages(t *testing.T) {
	cfg := config.Default().Crawl
	cfg.Overlap = config.Duration(10 * time.Second)
	since := t1.Add(-10 * time.Second)
	server := &replicationServer{t: t, pages: map[string]concrnt.QueryResult{
		// every row in the window was filtered out for the guest, next still moves
		stamp(since): {Items: nil, Prev: &t1, Next: &t2},
		stamp(t2):    {Items: nil, Prev: nil, Next: nil},
	}}
	store := &manualStore{}
	c := newReplicationCrawler(t, server, store, cfg)
	cursor := model.ReplicationCursor{ServerDomain: replicationDomain, CursorAt: &t1}
	if err := c.db.Create(&cursor).Error; err != nil {
		t.Fatal(err)
	}

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatal(err)
	}
	if len(server.requests) != 2 {
		t.Fatalf("expected two pages, got %v", server.requests)
	}
	if len(store.calls) != 0 {
		t.Fatalf("empty pages must not touch the store: %v", store.calls)
	}
	got := loadCursor(t, c)
	// the drained page had no prev: keep the last next rather than fall back
	if got.CursorAt == nil || !got.CursorAt.Equal(t2) {
		t.Fatalf("cursor should advance to the last next (%s), got %v", stamp(t2), got.CursorAt)
	}
}

func TestReplicateStepsPastStalledCursor(t *testing.T) {
	stepped := t1.Add(time.Microsecond)
	server := &replicationServer{t: t, pages: map[string]concrnt.QueryResult{
		"":             {Items: nil, Prev: &t0, Next: &t1},
		stamp(t1):      {Items: nil, Prev: &t1, Next: &t1},
		stamp(stepped): {Items: nil, Prev: &stepped, Next: nil},
	}}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatal(err)
	}
	if len(server.requests) != 3 {
		t.Fatalf("expected the stalled page to be stepped past, got %v", server.requests)
	}
}

func TestReplicateRejectsBackwardsCursor(t *testing.T) {
	server := &replicationServer{t: t, pages: map[string]concrnt.QueryResult{
		"":        {Items: nil, Prev: &t0, Next: &t1},
		stamp(t1): {Items: nil, Prev: &t1, Next: &t0},
	}}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)

	err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain})
	if err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("expected a backwards-cursor error, got %v", err)
	}
	cursor := loadCursor(t, c)
	if cursor.FailCount != 1 || cursor.LastErrorAt == nil {
		t.Fatalf("failure should be recorded on the cursor: %+v", cursor)
	}
	// the first page was applied before the bad one, so its next is kept
	if cursor.CursorAt == nil || !cursor.CursorAt.Equal(t1) {
		t.Fatalf("cursor should keep the last good next, got %v", cursor.CursorAt)
	}
}

func TestReplicateStopsAtMaxPagesPerRun(t *testing.T) {
	cfg := config.Default().Crawl
	cfg.MaxPagesPerRun = 1
	server := &replicationServer{t: t, pages: map[string]concrnt.QueryResult{
		"": {Items: nil, Prev: &t0, Next: &t1},
	}}
	c := newReplicationCrawler(t, server, &manualStore{}, cfg)
	cursor, err := c.getOrCreateReplicationCursor(context.Background(), replicationDomain)
	if err != nil {
		t.Fatal(err)
	}
	wkc, err := c.client.GetServer(context.Background(), replicationDomain, nil)
	if err != nil {
		t.Fatal(err)
	}

	caughtUp, err := c.replicate(context.Background(), wkc, &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if caughtUp {
		t.Fatal("a run capped by maxPagesPerRun is not caught up")
	}
	got := loadCursor(t, c)
	if got.CursorAt == nil || !got.CursorAt.Equal(t1) || got.CaughtUpAt != nil {
		t.Fatalf("cursor should hold the last next without caught_up_at: %+v", got)
	}
}

func TestCrawlServerTreatsRateLimitAsTransient(t *testing.T) {
	server := &replicationServer{t: t, status: http.StatusTooManyRequests}
	c := newReplicationCrawler(t, server, &manualStore{}, config.Default().Crawl)
	cursor := model.ReplicationCursor{ServerDomain: replicationDomain, CursorAt: &t1}
	if err := c.db.Create(&cursor).Error; err != nil {
		t.Fatal(err)
	}

	if err := c.crawlServer(context.Background(), model.ServerState{Domain: replicationDomain}); err != nil {
		t.Fatalf("429 must not be reported as a crawl failure: %v", err)
	}
	got := loadCursor(t, c)
	if got.FailCount != 0 || got.LastErrorAt != nil || got.CursorAt == nil || !got.CursorAt.Equal(t1) {
		t.Fatalf("cursor must be untouched after a 429: %+v", got)
	}
	var state model.ServerState
	if err := c.db.First(&state, "domain = ?", replicationDomain).Error; err != nil {
		t.Fatal(err)
	}
	if state.FailCount != 0 {
		t.Fatalf("server must not be marked failing after a 429: %+v", state)
	}
}
