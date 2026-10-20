package api

import (
	"net/url"
	"testing"
)

func TestSearchUsersQuerySyntax(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	code, body := get(t, h, "/api/v1/search/users?"+url.Values{"q": {"hello author:con1"}}.Encode())
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	// a profile's author is its ccid
	if store.index != "concrnt_users" || store.query != "hello" || store.filter != `ccid IN ["con1"]` {
		t.Fatalf("store got index=%q query=%q filter=%q", store.index, store.query, store.filter)
	}
	if body["query"] != "hello author:con1" {
		t.Fatalf("query = %v", body["query"])
	}
	if authors := stringList(body, "filters", "author"); len(authors) != 1 || authors[0] != "con1" {
		t.Fatalf("filters.author = %v", authors)
	}
	if _, ok := body["filters"].(map[string]any)["in"]; ok {
		t.Fatalf("users must not echo an in: key: %v", body["filters"])
	}

	// in: is not a term for users: it stays a word and yields no filter
	if code, _ := get(t, h, "/api/v1/search/users?"+url.Values{"q": {"hello in:cckv://a"}}.Encode()); code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.query != "hello in:cckv://a" || store.filter != "" {
		t.Fatalf("store got query=%q filter=%q", store.query, store.filter)
	}
}

func TestSearchCommunitiesQuerySyntax(t *testing.T) {
	store := &captureStore{}
	h := newViewerHandler(t, store)

	code, body := get(t, h, "/api/v1/search/communities?"+url.Values{"q": {"author:con1"}, "owner": {"example.net"}}.Encode())
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.index != "concrnt_communities" || store.query != "" || store.filter != `owner = "example.net" AND author IN ["con1"]` {
		t.Fatalf("store got index=%q query=%q filter=%q", store.index, store.query, store.filter)
	}
	if authors := stringList(body, "filters", "author"); len(authors) != 1 || authors[0] != "con1" {
		t.Fatalf("filters.author = %v", authors)
	}

	// the new author= param is a plain exact filter like on posts
	if code, _ := get(t, h, "/api/v1/search/communities?author=con1&q=chat"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if store.query != "chat" || store.filter != `author = "con1"` {
		t.Fatalf("store got query=%q filter=%q", store.query, store.filter)
	}
}
