package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/crawler"
	"github.com/concrnt/concrnt-crawler/internal/search/database"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/glebarez/sqlite"
	"github.com/labstack/echo/v4"
	meilisearch "github.com/meilisearch/meilisearch-go"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	viewer  = "con000000000000000000000000000000000000000"
	alice   = "con012345678901234567890123456789012345678"
	bob     = "con098765432109876543210987654321098765432"
	carol   = "con111111111111111111111111111111111111111"
	general = "cckv://example.com/concrnt.world/communities/general"
	quiet   = "cckv://example.com/concrnt.world/communities/quiet"
)

// fetchStore answers FetchDocuments from a fixed document set, filtering on
// the one attribute the viewer handlers use per index, and records the
// filters it was asked for.
type fetchStore struct {
	docs    map[string][]map[string]any
	filters []string
}

func (s *fetchStore) Search(context.Context, string, string, int64, int64, string, []string) (*meilisearch.SearchResponse, error) {
	return nil, nil
}

func (s *fetchStore) Stats(context.Context) (*meilisearch.Stats, error) {
	return nil, nil
}

func (s *fetchStore) FetchDocuments(_ context.Context, indexUID string, filter string, _ int64) ([]map[string]any, error) {
	s.filters = append(s.filters, filter)
	field, list, ok := strings.Cut(filter, " IN [")
	if !ok {
		return nil, nil
	}
	var out []map[string]any
	for _, doc := range s.docs[indexUID] {
		value, _ := doc[field].(string)
		if strings.Contains(list, `"`+value+`"`) {
			copied := map[string]any{}
			for k, v := range doc {
				copied[k] = v
			}
			out = append(out, copied)
		}
	}
	return out, nil
}

func newViewerHandler(t *testing.T, store SearchStore) *Handler {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return New(db, store, nil, 7*24*time.Hour, []string{config.DefaultAckSchema})
}

func get(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	e := echo.New()
	e.Debug = true
	h.RegisterRoutes(e)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code >= http.StatusInternalServerError {
		t.Logf("%s: %s", path, rec.Body.String())
	}
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, body
}

func hits(body map[string]any) []map[string]any {
	raw, _ := body["hits"].([]any)
	out := make([]map[string]any, len(raw))
	for i, hit := range raw {
		out[i], _ = hit.(map[string]any)
	}
	return out
}

func userDoc(ccid string, profile string) map[string]any {
	return map[string]any{"ccid": ccid, "cckv": "cckv://" + ccid + "/concrnt.world/profiles/" + profile, "username": ccid[:6] + "/" + profile}
}

func seedFollows(t *testing.T, h *Handler, now time.Time) {
	t.Helper()
	day := 24 * time.Hour
	if err := h.db.Create([]model.Ack{
		{Acker: viewer, Ackee: alice, Schema: config.DefaultAckSchema, CreatedAt: now, Valid: true},
		{Acker: viewer, Ackee: bob, Schema: config.DefaultAckSchema, CreatedAt: now, Valid: true},
		// unfollowed, and a follow with another schema: neither counts
		{Acker: viewer, Ackee: carol, Schema: config.DefaultAckSchema, CreatedAt: now, Valid: false},
		{Acker: viewer, Ackee: carol, Schema: "https://schema.concrnt.world/ack/other.json", CreatedAt: now, Valid: true},
		// someone else's follow
		{Acker: alice, Ackee: carol, Schema: config.DefaultAckSchema, CreatedAt: now, Valid: true},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create([]model.IndexedUser{
		{CCKV: "cckv://" + alice + "/concrnt.world/profiles/main", CCID: alice, IndexedAt: now},
		{CCKV: "cckv://" + alice + "/concrnt.world/profiles/work", CCID: alice, IndexedAt: now},
		{CCKV: "cckv://" + bob + "/concrnt.world/profiles/main", CCID: bob, IndexedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create([]model.UserEntry{
		{Author: alice, EntryCCKV: "cckv://" + alice + "/p1", CreatedAt: now.Add(-1 * day)},
		{Author: alice, EntryCCKV: "cckv://" + alice + "/p2", CreatedAt: now.Add(-40 * day)}, // out of the window
		{Author: bob, EntryCCKV: "cckv://" + bob + "/p1", CreatedAt: now.Add(-7 * day)},
		{Author: bob, EntryCCKV: "cckv://" + bob + "/p2", CreatedAt: now.Add(-7 * day)},
		{Author: bob, EntryCCKV: "cckv://" + bob + "/p3", CreatedAt: now.Add(-7 * day)},
		{Author: carol, EntryCCKV: "cckv://" + carol + "/p1", CreatedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create([]model.IndexedCommunity{{CCKV: general, IndexedAt: now}, {CCKV: quiet, IndexedAt: now}}).Error; err != nil {
		t.Fatal(err)
	}
	gone := "cckv://example.com/concrnt.world/communities/gone"
	if err := h.db.Create([]model.CommunityEntry{
		{CommunityCCKV: general, EntryCCKV: general + "/1", Author: alice, CreatedAt: now.Add(-1 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/2", Author: bob, CreatedAt: now.Add(-2 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/3", Author: bob, CreatedAt: now.Add(-3 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/4", Author: carol, CreatedAt: now},
		{CommunityCCKV: quiet, EntryCCKV: quiet + "/1", Author: alice, CreatedAt: now.Add(-20 * day)},
		{CommunityCCKV: gone, EntryCCKV: gone + "/1", Author: alice, CreatedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestFolloweeUsers(t *testing.T) {
	store := &fetchStore{docs: map[string][]map[string]any{
		meili.UsersIndex: {userDoc(alice, "work"), userDoc(alice, "main"), userDoc(bob, "main"), userDoc(carol, "main")},
	}}
	h := newViewerHandler(t, store)
	now := time.Now().UTC()
	seedFollows(t, h, now)

	code, body := get(t, h, "/api/v1/search/users?viewer="+viewer)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got := hits(body)
	// bob: three posts at 7d = 1.5; alice: one post at 1d ≈ 0.906
	if len(got) != 2 || got[0]["ccid"] != bob || got[1]["ccid"] != alice {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[0]["followeePostCount30d"] != float64(3) || math.Abs(got[0]["followeeScore"].(float64)-1.5) > 1e-6 {
		t.Fatalf("unexpected bob score: %+v", got[0])
	}
	if got[1]["followeePostCount30d"] != float64(1) {
		t.Fatalf("alice's old post must not count: %+v", got[1])
	}
	// one document per ccid, the main profile
	if got[1]["cckv"] != "cckv://"+alice+"/concrnt.world/profiles/main" {
		t.Fatalf("main profile should be picked: %+v", got[1])
	}
	if body["estimatedTotalHits"] != float64(2) || body["query"] != "" {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	if len(store.filters) != 1 || !strings.HasPrefix(store.filters[0], "ccid IN [") {
		t.Fatalf("documents should be fetched by ccid: %v", store.filters)
	}

	// paging over the ranked list
	code, body = get(t, h, "/api/v1/search/users?viewer="+viewer+"&limit=1&offset=1")
	if got := hits(body); code != http.StatusOK || len(got) != 1 || got[0]["ccid"] != alice || body["estimatedTotalHits"] != float64(2) {
		t.Fatalf("unexpected second page: %d %+v", code, body)
	}
	code, body = get(t, h, "/api/v1/search/users?viewer="+viewer+"&offset=5")
	if got := hits(body); code != http.StatusOK || len(got) != 0 || body["estimatedTotalHits"] != float64(2) {
		t.Fatalf("offset past the end should be empty: %d %+v", code, body)
	}

	// a followee without an indexed profile is not ranked
	if err := h.db.Where("cckv LIKE ?", "cckv://"+bob+"%").Delete(&model.IndexedUser{}).Error; err != nil {
		t.Fatal(err)
	}
	_, body = get(t, h, "/api/v1/search/users?viewer="+viewer)
	if got := hits(body); len(got) != 1 || got[0]["ccid"] != alice || body["estimatedTotalHits"] != float64(1) {
		t.Fatalf("unindexed followee should be excluded: %+v", body)
	}

	// a viewer following nobody gets the empty envelope without a fetch
	store.filters = nil
	code, body = get(t, h, "/api/v1/search/users?viewer="+carol)
	if got := hits(body); code != http.StatusOK || len(got) != 0 || body["estimatedTotalHits"] != float64(0) || len(store.filters) != 0 {
		t.Fatalf("no followees should yield empty hits without a fetch: %d %+v %v", code, body, store.filters)
	}
}

func TestFolloweeCommunities(t *testing.T) {
	store := &fetchStore{docs: map[string][]map[string]any{
		meili.CommunitiesIndex: {
			{"cckv": general, "name": "general"},
			{"cckv": quiet, "name": "quiet"},
		},
	}}
	h := newViewerHandler(t, store)
	now := time.Now().UTC()
	seedFollows(t, h, now)

	code, body := get(t, h, "/api/v1/search/communities?viewer="+viewer)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got := hits(body)
	if len(got) != 2 || got[0]["cckv"] != general || got[1]["cckv"] != quiet {
		t.Fatalf("unexpected order: %+v", got)
	}
	// carol's entry is not a followee's; the gone community is not indexed
	if got[0]["followeePostCount30d"] != float64(3) || got[0]["name"] != "general" {
		t.Fatalf("unexpected general hit: %+v", got[0])
	}
	wantScore := math.Exp2(-1.0/7) + math.Exp2(-2.0/7) + math.Exp2(-3.0/7)
	if math.Abs(got[0]["followeeScore"].(float64)-wantScore) > 1e-6 {
		t.Fatalf("score = %v want %v", got[0]["followeeScore"], wantScore)
	}
	authors, _ := got[0]["topAuthors"].([]any)
	if len(authors) != 2 || authors[0] != bob || authors[1] != alice {
		t.Fatalf("topAuthors should list followees by entry count: %+v", authors)
	}
	if authors, _ := got[1]["topAuthors"].([]any); len(authors) != 1 || authors[0] != alice {
		t.Fatalf("unexpected quiet topAuthors: %+v", authors)
	}
	if body["estimatedTotalHits"] != float64(2) {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}

func TestTopAuthorsCapAndTies(t *testing.T) {
	counts := map[string]int{"g": 1, "f": 2, "e": 2, "d": 3, "c": 3, "b": 3, "a": 1}
	got := topAuthors(counts)
	want := []string{"b", "c", "d", "e", "f"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("topAuthors = %v want %v", got, want)
	}
}

func TestViewerModeRejectsBadRequests(t *testing.T) {
	h := newViewerHandler(t, &fetchStore{})
	for _, path := range []string{
		"/api/v1/search/users?viewer=not-a-ccid",
		"/api/v1/search/users?viewer=" + viewer + "&q=hello",
		"/api/v1/search/users?viewer=" + viewer + "&sort=createdAt",
		"/api/v1/search/communities?viewer=example.com",
		"/api/v1/search/communities?viewer=" + viewer + "&sort=activityScore",
	} {
		if code, _ := get(t, h, path); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, code)
		}
	}
	// the activity window shared with the tick is the one the ranking uses
	if crawler.ActivityWindow != 30*24*time.Hour {
		t.Fatalf("unexpected window %v", crawler.ActivityWindow)
	}
}
