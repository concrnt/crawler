package api

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
)

// TestSearchUsersCommunitiesAgainstMeilisearch runs the users and
// communities endpoints against a live Meilisearch (CRAWLER_TEST_MEILI_HOST /
// CRAWLER_TEST_MEILI_KEY) with test-only documents, checking that `author:`
// filters on ccid for profiles and on the new author attribute for
// communities, then removes them.
func TestSearchUsersCommunitiesAgainstMeilisearch(t *testing.T) {
	host := os.Getenv("CRAWLER_TEST_MEILI_HOST")
	if host == "" {
		t.Skip("CRAWLER_TEST_MEILI_HOST not set")
	}
	const (
		ann = "con0a0000000000000000000000000000000000000"
		ben = "con0b0000000000000000000000000000000000000"
	)
	ctx := context.Background()
	store := meili.New(meili.NewClient(host, os.Getenv("CRAWLER_TEST_MEILI_KEY")), 0, slog.Default())
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	users := []normalize.UserDocument{
		{ID: "qsyntax-user-ann-main", Type: "user", CCKV: "cckv://" + ann + "/concrnt.world/profiles/main", CCID: ann, Owner: ann, Username: "qsyntax ann", Subprofiles: []string{}, Badges: []normalize.ProfileBadge{}, Ancestors: []string{}, CreatedAt: at, IndexedAt: at},
		{ID: "qsyntax-user-ann-work", Type: "user", CCKV: "cckv://" + ann + "/concrnt.world/profiles/work", CCID: ann, Owner: ann, Username: "qsyntax ann at work", Subprofiles: []string{}, Badges: []normalize.ProfileBadge{}, Ancestors: []string{}, CreatedAt: at, IndexedAt: at},
		{ID: "qsyntax-user-ben", Type: "user", CCKV: "cckv://" + ben + "/concrnt.world/profiles/main", CCID: ben, Owner: ben, Username: "qsyntax ben", Subprofiles: []string{}, Badges: []normalize.ProfileBadge{}, Ancestors: []string{}, CreatedAt: at, IndexedAt: at},
	}
	communities := []normalize.CommunityDocument{
		{ID: "qsyntax-community-ann", Type: "community", CCKV: "cckv://qsyntax.test/concrnt.world/communities/ann", Author: ann, Owner: "qsyntax.test", Name: "qsyntax room by ann", Ancestors: []string{}, CreatedAt: at, IndexedAt: at},
		{ID: "qsyntax-community-ben", Type: "community", CCKV: "cckv://qsyntax.test/concrnt.world/communities/ben", Author: ben, Owner: "qsyntax.test", Name: "qsyntax room by ben", Ancestors: []string{}, CreatedAt: at, IndexedAt: at},
	}
	t.Cleanup(func() {
		_ = store.DeleteRecords(ctx, meili.UsersIndex, meili.DeleteSpec{IDs: []string{"qsyntax-user-ann-main", "qsyntax-user-ann-work", "qsyntax-user-ben"}})
		_ = store.DeleteRecords(ctx, meili.CommunitiesIndex, meili.DeleteSpec{IDs: []string{"qsyntax-community-ann", "qsyntax-community-ben"}})
	})
	if err := store.UpsertUsers(ctx, users); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCommunities(ctx, communities); err != nil {
		t.Fatal(err)
	}

	h := newViewerHandler(t, store)
	field := func(endpoint string, q string, key string) []string {
		t.Helper()
		code, body := get(t, h, "/api/v1/search/"+endpoint+"?"+url.Values{"q": {q}}.Encode())
		if code != 200 {
			t.Fatalf("%s %s: status %d", endpoint, q, code)
		}
		out := []string{}
		for _, hit := range hits(body) {
			if v, _ := hit[key].(string); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	// users: both of ann's profiles, nobody else
	if got := field("users", "qsyntax author:"+ann, "ccid"); len(got) != 2 || got[0] != ann || got[1] != ann {
		t.Fatalf("users author: got %v", got)
	}
	if got := field("users", "author:"+ben, "username"); len(got) != 1 || got[0] != "qsyntax ben" {
		t.Fatalf("users bare author: got %v", got)
	}
	// communities: by the creator's ccid, text narrows further
	if got := field("communities", "qsyntax author:"+ann, "name"); len(got) != 1 || got[0] != "qsyntax room by ann" {
		t.Fatalf("communities author: got %v", got)
	}
	if got := field("communities", "author:"+ann+" author:"+ben+" qsyntax", "name"); len(got) != 2 {
		t.Fatalf("communities two authors: got %v", got)
	}
}
