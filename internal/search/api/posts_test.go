package api

import (
	"context"
	"net/url"
	"testing"

	meilisearch "github.com/meilisearch/meilisearch-go"
)

// captureStore records what the posts handler asks Meilisearch for.
type captureStore struct {
	index  string
	query  string
	filter string
	sort   []string
}

func (s *captureStore) Search(_ context.Context, indexUID string, query string, _ int64, _ int64, filter string, sort []string) (*meilisearch.SearchResponse, error) {
	s.index, s.query, s.filter, s.sort = indexUID, query, filter, sort
	return &meilisearch.SearchResponse{Hits: []any{}}, nil
}

func (s *captureStore) FetchDocuments(context.Context, string, string, int64) ([]map[string]any, error) {
	return nil, nil
}

func (s *captureStore) Stats(context.Context) (*meilisearch.Stats, error) {
	return &meilisearch.Stats{}, nil
}

func postsPath(params url.Values) string {
	return "/api/v1/search/posts?" + params.Encode()
}

func stringList(body map[string]any, keys ...string) []any {
	var cur any = body
	for _, key := range keys {
		m, _ := cur.(map[string]any)
		cur = m[key]
	}
	list, _ := cur.([]any)
	return list
}

func TestSearchPostsQuerySyntax(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	code, body := get(t, h, postsPath(url.Values{"q": {"hello author:con1 in:cckv://a"}, "sourceServer": {"example.net"}}))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.index != "concrnt_posts" || store.query != "hello" {
		t.Fatalf("store got index=%q query=%q", store.index, store.query)
	}
	if want := `sourceServer = "example.net" AND author IN ["con1"] AND timelines IN ["cckv://a"]`; store.filter != want {
		t.Fatalf("filter = %q, want %q", store.filter, want)
	}
	if len(store.sort) != 1 || store.sort[0] != "createdAt:desc" {
		t.Fatalf("default sort must stay createdAt:desc, got %v", store.sort)
	}
	// the response echoes q as received and the parsed terms
	if body["query"] != "hello author:con1 in:cckv://a" {
		t.Fatalf("query = %v", body["query"])
	}
	if authors := stringList(body, "filters", "author"); len(authors) != 1 || authors[0] != "con1" {
		t.Fatalf("filters.author = %v", authors)
	}
	if timelines := stringList(body, "filters", "in"); len(timelines) != 1 || timelines[0] != "cckv://a" {
		t.Fatalf("filters.in = %v", timelines)
	}
}

func TestSearchPostsTermsOnly(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	// a bare author: term lists that author's posts (empty text, newest first)
	if code, _ := get(t, h, postsPath(url.Values{"q": {"author:con1"}})); code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.query != "" || store.filter != `author IN ["con1"]` {
		t.Fatalf("store got query=%q filter=%q", store.query, store.filter)
	}
}

func TestSearchPostsWithoutTerms(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	code, body := get(t, h, postsPath(url.Values{"q": {"hello"}}))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.query != "hello" || store.filter != "" {
		t.Fatalf("store got query=%q filter=%q", store.query, store.filter)
	}
	// filters is always present with empty arrays, never null
	filters, ok := body["filters"].(map[string]any)
	if !ok {
		t.Fatalf("filters missing: %v", body)
	}
	for _, key := range []string{"author", "in"} {
		list, ok := filters[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("filters.%s = %v, want []", key, filters[key])
		}
	}
}

func TestSearchPostsTermAndParamAreAnded(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	if code, _ := get(t, h, postsPath(url.Values{"q": {"author:con2"}, "author": {"con1"}})); code != 200 {
		t.Fatalf("status %d", code)
	}
	if want := `author = "con1" AND author IN ["con2"]`; store.filter != want {
		t.Fatalf("filter = %q, want %q", store.filter, want)
	}
}
