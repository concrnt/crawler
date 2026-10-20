package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
)

// TestSearchPostsAgainstMeilisearch runs the posts endpoint against a live
// Meilisearch named by CRAWLER_TEST_MEILI_HOST (and CRAWLER_TEST_MEILI_KEY):
// it writes test-only posts into the real posts index, checks that the
// `author:` / `in:` terms select exactly those, and removes them.
func TestSearchPostsAgainstMeilisearch(t *testing.T) {
	host := os.Getenv("CRAWLER_TEST_MEILI_HOST")
	if host == "" {
		t.Skip("CRAWLER_TEST_MEILI_HOST not set")
	}
	const postSchema = "https://schema.concrnt.world/m/markdown.json"
	const (
		ann  = "con0a0000000000000000000000000000000000000"
		ben  = "con0b0000000000000000000000000000000000000"
		home = "cckv://" + ann + "/concrnt.world/profiles/main/home-timeline"
		room = "cckv://qsyntax.test/concrnt.world/communities/room"
	)
	ctx := context.Background()
	store := meili.New(meili.NewClient(host, os.Getenv("CRAWLER_TEST_MEILI_KEY")), 0, slog.Default())
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}

	post := func(author string, n string, body string, at time.Time, distributes []string) normalize.PostDocument {
		key := "cckv://" + author + "/concrnt.world/profiles/main/posts/qsyntax-" + n
		raw, err := json.Marshal(concrnt.Document[any]{
			Kind: "record", Key: key, Value: map[string]any{"body": body},
			Author: author, Schema: postSchema, Distributes: &distributes, CreatedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		doc, ok, err := normalize.NormalizePost(concrnt.SignedDocument{Document: string(raw), Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}}, postSchema, "qsyntax.test", at)
		if err != nil || !ok {
			t.Fatalf("normalize: ok=%v err=%v", ok, err)
		}
		return doc
	}
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	docs := []normalize.PostDocument{
		post(ann, "1", "qsyntax apple in room", base, []string{home, room}),
		post(ann, "2", "qsyntax apple at home only", base.Add(time.Minute), []string{home}),
		post(ben, "3", "qsyntax apple from ben in room", base.Add(2*time.Minute), []string{room}),
		post(ben, "4", "qsyntax banana nowhere", base.Add(3*time.Minute), nil),
	}
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.ID
	}
	t.Cleanup(func() { _ = store.DeleteRecords(ctx, meili.PostsIndex, meili.DeleteSpec{IDs: ids}) })
	if err := store.UpsertPosts(ctx, docs); err != nil {
		t.Fatal(err)
	}

	h := newViewerHandler(t, store)
	bodies := func(q string, params url.Values) []string {
		t.Helper()
		params.Set("q", q)
		code, body := get(t, h, postsPath(params))
		if code != 200 {
			t.Fatalf("%s: status %d", q, code)
		}
		out := []string{}
		for _, hit := range hits(body) {
			if b, _ := hit["body"].(string); b != "" {
				out = append(out, b)
			}
		}
		return out
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// text + in: narrows to the room's posts, newest first
	if got := bodies("qsyntax apple in:"+room, url.Values{}); !same(got, "qsyntax apple from ben in room", "qsyntax apple in room") {
		t.Fatalf("in: got %v", got)
	}
	// author: alone lists that author's posts
	if got := bodies("author:"+ann, url.Values{}); !same(got, "qsyntax apple at home only", "qsyntax apple in room") {
		t.Fatalf("author: got %v", got)
	}
	// both terms are ANDed
	if got := bodies("author:"+ann+" in:"+room, url.Values{}); !same(got, "qsyntax apple in room") {
		t.Fatalf("author+in: got %v", got)
	}
	// two in: values are ORed
	if got := bodies("qsyntax in:"+home+" in:"+room, url.Values{}); !same(got, "qsyntax apple from ben in room", "qsyntax apple at home only", "qsyntax apple in room") {
		t.Fatalf("in OR in: got %v", got)
	}
	// a plain filter param still ANDs with the terms
	if got := bodies("in:"+room, url.Values{"author": {ben}}); !same(got, "qsyntax apple from ben in room") {
		t.Fatalf("author param + in: got %v", got)
	}
	// a post with no distributes never matches in:
	if got := bodies("banana in:"+room, url.Values{}); len(got) != 0 {
		t.Fatalf("no-distributes post matched: %v", got)
	}
}
