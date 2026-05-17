package crawler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	"github.com/concrnt/concrnt/client"
)

func TestCrawlCCFSIndexesProfile(t *testing.T) {
	const domain = "manual.test"
	const ccfs = "ccfs://manual.test/bafyexample"

	sd := testSignedDocument(t, time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC))
	store := &manualStore{}
	cl := client.New(domain)
	cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/concrnt":
			return jsonResponse(t, concrnt.WellKnownConcrnt{
				Domain: domain,
				Endpoints: map[string]string{
					"net.concrnt.core.resolve": "/resolve?uri={uri}",
				},
			})
		case "/resolve":
			uri, err := url.QueryUnescape(r.URL.Query().Get("uri"))
			if err != nil {
				t.Fatal(err)
			}
			if uri != ccfs {
				t.Fatalf("resolve uri mismatch: %s", r.URL.RawQuery)
			}
			return jsonResponse(t, sd)
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}
	})

	c := New(nil, store, cl, config.Default().Crawl, nil)
	result, err := c.CrawlCCFS(context.Background(), ccfs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != KindProfile || result.CCFS != ccfs || result.SourceServer != domain {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(store.users) != 1 {
		t.Fatalf("expected one user upsert, got %d", len(store.users))
	}
	if store.users[0].CCFS != ccfs {
		t.Fatalf("ccfs mismatch: %s", store.users[0].CCFS)
	}
}

type manualStore struct {
	users       []normalize.UserDocument
	communities []normalize.CommunityDocument
}

func (s *manualStore) UpsertServers(context.Context, []meili.ServerDocument) error {
	return nil
}

func (s *manualStore) UpsertUsers(_ context.Context, docs []normalize.UserDocument) error {
	s.users = append(s.users, docs...)
	return nil
}

func (s *manualStore) UpsertCommunities(_ context.Context, docs []normalize.CommunityDocument) error {
	s.communities = append(s.communities, docs...)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(t *testing.T, value any) (*http.Response, error) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}
